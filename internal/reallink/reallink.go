package reallink

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/honghanh691913-glitch/Cloudflare-BestIP/internal/config"
)

func Available() bool {
	_, err := exec.LookPath(binary())
	return err == nil
}

func binary() string {
	if v := strings.TrimSpace(os.Getenv("BESTIP_SINGBOX")); v != "" {
		return v
	}
	return "sing-box"
}

func ParseURI(raw string) (config.RealProfile, error) {
	var p config.RealProfile
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return p, errors.New("节点链接为空")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return p, err
	}
	if strings.ToLower(u.Scheme) != "vless" {
		return p, fmt.Errorf("当前真连接测试先支持 VLESS，收到 %s", u.Scheme)
	}
	if u.User == nil || strings.TrimSpace(u.User.Username()) == "" {
		return p, errors.New("VLESS 缺少 UUID")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		return p, errors.New("VLESS 端口无效")
	}
	q := u.Query()
	p.Name, _ = url.PathUnescape(u.Fragment)
	if strings.TrimSpace(p.Name) == "" {
		p.Name = "VLESS 节点"
	}
	p.Protocol = "vless"
	p.Server = u.Hostname()
	p.Port = port
	p.UUID = u.User.Username()
	p.Encryption = firstNonEmpty(q.Get("encryption"), "none")
	p.Flow = q.Get("flow")
	p.Network = strings.ToLower(firstNonEmpty(q.Get("type"), "ws"))
	p.Security = strings.ToLower(firstNonEmpty(q.Get("security"), "tls"))
	p.SNI = q.Get("sni")
	p.Host = q.Get("host")
	p.Path = firstNonEmpty(q.Get("path"), "/")
	p.Fingerprint = firstNonEmpty(q.Get("fp"), "chrome")
	p.ALPN = q.Get("alpn")
	p.Insecure = truthy(q.Get("insecure")) || truthy(q.Get("allowInsecure"))
	p.ECH = q.Get("ech")
	p.RawURI = raw
	if p.ECH != "" {
		parts := strings.SplitN(p.ECH, "+", 2)
		p.ECHQueryName = strings.TrimSpace(parts[0])
		if len(parts) > 1 {
			p.ECHDoH = strings.TrimSpace(parts[1])
		}
	}
	if p.Network != "ws" {
		return p, fmt.Errorf("当前 v0.7 真连接核心先支持 VLESS + WebSocket；该节点 type=%s", p.Network)
	}
	if p.Security == "tls" && p.SNI == "" {
		p.SNI = p.Host
	}
	return p, ValidateProfile(p)
}

func ValidateProfile(p config.RealProfile) error {
	if strings.ToLower(strings.TrimSpace(p.Protocol)) != "vless" {
		return errors.New("当前仅支持 VLESS")
	}
	if strings.TrimSpace(p.Server) == "" || p.Port < 1 || p.Port > 65535 {
		return errors.New("节点地址或端口无效")
	}
	if strings.TrimSpace(p.UUID) == "" {
		return errors.New("节点 UUID 为空")
	}
	if n := strings.ToLower(strings.TrimSpace(p.Network)); n != "" && n != "ws" {
		return errors.New("当前仅支持 WebSocket 传输")
	}
	if strings.ToLower(strings.TrimSpace(p.Security)) == "tls" && strings.TrimSpace(firstNonEmpty(p.SNI, p.Host)) == "" {
		return errors.New("TLS 节点需要 SNI 或 Host")
	}
	return nil
}

type Result struct {
	LatencyMS float64 `json:"latency_ms"`
	SpeedMB   float64 `json:"speed_mb,omitempty"`
}

type LatencyResult struct {
	Candidate string  `json:"candidate"`
	LatencyMS float64 `json:"latency_ms,omitempty"`
	Error     string  `json:"error,omitempty"`
}

func MeasureLatency(ctx context.Context, p config.RealProfile, candidate, testURL string, attempts int) (float64, error) {
	results, err := MeasureLatenciesBatch(ctx, p, []string{candidate}, testURL, attempts, 1, nil)
	if err != nil {
		return 0, err
	}
	if len(results) != 1 {
		return 0, errors.New("没有取得真连接延迟")
	}
	if results[0].Error != "" {
		return 0, errors.New(results[0].Error)
	}
	return results[0].LatencyMS, nil
}

// MeasureLatenciesBatch keeps all candidate outbounds in one sing-box process.
// This shares core startup and DNS/ECH caches across the batch, like a desktop
// proxy client's batch delay test, instead of cold-starting a core per IP.
func MeasureLatenciesBatch(
	ctx context.Context,
	p config.RealProfile,
	candidates []string,
	testURL string,
	attempts int,
	concurrency int,
	onResult func(LatencyResult, int, int),
) ([]LatencyResult, error) {
	if err := ValidateProfile(p); err != nil {
		return nil, err
	}
	if !Available() {
		return nil, fmt.Errorf("sing-box 不可用：请更新到包含真连接核心的 Docker 镜像")
	}
	if len(candidates) == 0 {
		return []LatencyResult{}, nil
	}
	if strings.TrimSpace(testURL) == "" {
		testURL = config.DefaultRealTestURL
	}
	if attempts < 1 {
		attempts = 1
	}
	if attempts > 5 {
		attempts = 5
	}
	if concurrency < 1 {
		concurrency = 1
	}
	if concurrency > 16 {
		concurrency = 16
	}
	if concurrency > len(candidates) {
		concurrency = len(candidates)
	}

	// One extra outbound uses the saved original address for a non-counted
	// warm-up, which primes DNS/ECH and core state before timing candidates.
	allCandidates := make([]string, 0, len(candidates)+1)
	allCandidates = append(allCandidates, p.Server)
	allCandidates = append(allCandidates, candidates...)

	ports, err := freePorts(len(allCandidates))
	if err != nil {
		return nil, err
	}
	cfg, err := BuildBatchConfig(p, allCandidates, ports)
	if err != nil {
		return nil, err
	}

	dir, err := os.MkdirTemp("", "bestip-reallink-batch-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "config.json")
	b, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(path, b, 0600); err != nil {
		return nil, err
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var logs lockedBuffer
	cmd := exec.CommandContext(runCtx, binary(), "run", "-c", path)
	cmd.Stdout = &logs
	cmd.Stderr = &logs
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	defer func() {
		cancel()
		select {
		case <-waitCh:
		case <-time.After(1200 * time.Millisecond):
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
		}
	}()

	if err := waitPortsReady(ctx, ports, waitCh, &logs, 6*time.Second); err != nil {
		return nil, err
	}

	// Global warm-up through the saved node. It is intentionally not timed.
	warmProxy, _ := url.Parse("http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(ports[0])))
	_ = doLatencyRequest(ctx, warmProxy, testURL, 8*time.Second)

	type job struct {
		index int
		ip    string
		port  int
	}
	type item struct {
		index int
		res   LatencyResult
	}
	jobs := make(chan job)
	done := make(chan item, len(candidates))
	var wg sync.WaitGroup

	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				res := LatencyResult{Candidate: j.ip}
				proxyURL, _ := url.Parse("http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(j.port)))

				// Candidate-specific first connection is also a warm-up. This
				// removes first-use ECH/TLS/WS initialization from the reported
				// latency while formal samples still create fresh connections.
				_ = doLatencyRequest(ctx, proxyURL, testURL, 10*time.Second)

				values := make([]float64, 0, attempts)
				var lastErr error
				for i := 0; i < attempts; i++ {
					if err := ctx.Err(); err != nil {
						lastErr = err
						break
					}
					start := time.Now()
					if err := doLatencyRequest(ctx, proxyURL, testURL, 10*time.Second); err != nil {
						lastErr = err
						break
					}
					values = append(values, float64(time.Since(start).Microseconds())/1000)
				}
				if len(values) == 0 {
					if lastErr == nil {
						lastErr = errors.New("没有取得真连接延迟")
					}
					res.Error = lastErr.Error()
				} else {
					sort.Float64s(values)
					if len(values)%2 == 1 {
						res.LatencyMS = values[len(values)/2]
					} else {
						n := len(values)
						res.LatencyMS = (values[n/2-1] + values[n/2]) / 2
					}
				}
				done <- item{index: j.index, res: res}
			}
		}()
	}

	go func() {
		for i, ip := range candidates {
			select {
			case <-ctx.Done():
				close(jobs)
				wg.Wait()
				close(done)
				return
			case jobs <- job{index: i, ip: ip, port: ports[i+1]}:
			}
		}
		close(jobs)
		wg.Wait()
		close(done)
	}()

	results := make([]LatencyResult, len(candidates))
	completed := 0
	for x := range done {
		results[x.index] = x.res
		completed++
		if onResult != nil {
			onResult(x.res, completed, len(candidates))
		}
	}
	if err := ctx.Err(); err != nil {
		return results, err
	}
	return results, nil
}

func doLatencyRequest(ctx context.Context, proxyURL *url.URL, testURL string, timeout time.Duration) error {
	tr := &http.Transport{
		Proxy:                 http.ProxyURL(proxyURL),
		DisableKeepAlives:     true,
		TLSHandshakeTimeout:   6 * time.Second,
		ResponseHeaderTimeout: 8 * time.Second,
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: timeout}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, testURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "BestIP-RealLink/2")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64*1024))
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("真连接地址返回 HTTP %s", resp.Status)
	}
	return nil
}

func MeasureSpeed(ctx context.Context, p config.RealProfile, candidate, speedURL string, bytesMB int) (float64, error) {
	if strings.TrimSpace(speedURL) == "" {
		speedURL = config.DefaultSpeedURL
	}
	if bytesMB < 1 {
		bytesMB = 5
	}
	if bytesMB > 100 {
		bytesMB = 100
	}
	limit := int64(bytesMB) * 1024 * 1024
	var measured float64
	err := withProxy(ctx, p, candidate, func(proxyURL *url.URL) error {
		tr := &http.Transport{
			Proxy:                 http.ProxyURL(proxyURL),
			DisableKeepAlives:     true,
			TLSHandshakeTimeout:   8 * time.Second,
			ResponseHeaderTimeout: 10 * time.Second,
		}
		defer tr.CloseIdleConnections()
		client := &http.Client{Transport: tr, Timeout: 30 * time.Second}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, speedURL, nil)
		if err != nil {
			return err
		}
		req.Header.Set("User-Agent", "BestIP-RealSpeed/1")
		start := time.Now()
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 400 {
			return fmt.Errorf("真测速地址返回 HTTP %s", resp.Status)
		}
		n, err := io.Copy(io.Discard, io.LimitReader(resp.Body, limit))
		elapsed := time.Since(start).Seconds()
		if err != nil && n == 0 {
			return err
		}
		if n <= 0 || elapsed <= 0 {
			return errors.New("真测速没有收到数据")
		}
		measured = float64(n) / elapsed / 1024 / 1024
		return nil
	})
	return measured, err
}

func withProxy(ctx context.Context, p config.RealProfile, candidate string, fn func(*url.URL) error) error {
	if err := ValidateProfile(p); err != nil {
		return err
	}
	if !Available() {
		return fmt.Errorf("sing-box 不可用：请更新到包含真连接核心的 Docker 镜像")
	}
	if strings.TrimSpace(candidate) == "" {
		candidate = p.Server
	}
	port, err := freePort()
	if err != nil {
		return err
	}
	cfg, err := BuildConfig(p, candidate, port)
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "bestip-reallink-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "config.json")
	b, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(path, b, 0600); err != nil {
		return err
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var logs lockedBuffer
	cmd := exec.CommandContext(runCtx, binary(), "run", "-c", path)
	cmd.Stdout = &logs
	cmd.Stderr = &logs
	if err := cmd.Start(); err != nil {
		return err
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	defer func() {
		cancel()
		select {
		case <-waitCh:
		case <-time.After(1200 * time.Millisecond):
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
		}
	}()

	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	deadline := time.Now().Add(4 * time.Second)
	for {
		select {
		case err := <-waitCh:
			if err == nil {
				err = errors.New("sing-box 提前退出")
			}
			return fmt.Errorf("%v: %s", err, logs.Tail())
		default:
		}
		conn, err := net.DialTimeout("tcp", addr, 120*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("sing-box 本地代理启动超时: %s", logs.Tail())
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(80 * time.Millisecond):
		}
	}
	proxyURL, _ := url.Parse("http://" + addr)
	return fn(proxyURL)
}

func BuildBatchConfig(p config.RealProfile, candidates []string, localPorts []int) (map[string]any, error) {
	if err := ValidateProfile(p); err != nil {
		return nil, err
	}
	if len(candidates) == 0 || len(candidates) != len(localPorts) {
		return nil, errors.New("批量真连接参数不完整")
	}

	inbounds := make([]any, 0, len(candidates))
	outbounds := make([]any, 0, len(candidates))
	rules := make([]any, 0, len(candidates))
	for i, candidate := range candidates {
		inTag := fmt.Sprintf("mixed-%d", i)
		outTag := fmt.Sprintf("test-out-%d", i)
		out, err := buildOutbound(p, candidate, outTag)
		if err != nil {
			return nil, err
		}
		inbounds = append(inbounds, map[string]any{
			"type": "mixed", "tag": inTag, "listen": "127.0.0.1", "listen_port": localPorts[i],
		})
		outbounds = append(outbounds, out)
		rules = append(rules, map[string]any{
			"inbound": []string{inTag}, "action": "route", "outbound": outTag,
		})
	}
	cfg := map[string]any{
		"log":       map[string]any{"level": "warn", "timestamp": false},
		"inbounds":  inbounds,
		"outbounds": outbounds,
		"route": map[string]any{
			"rules": rules, "final": "test-out-0", "auto_detect_interface": true,
		},
	}
	attachDNSConfig(cfg, p)
	return cfg, nil
}

func buildOutbound(p config.RealProfile, candidate, tag string) (map[string]any, error) {
	if strings.TrimSpace(candidate) == "" {
		return nil, errors.New("候选地址为空")
	}
	tlsObj := map[string]any{}
	if strings.ToLower(p.Security) == "tls" {
		serverName := firstNonEmpty(p.SNI, p.Host)
		tlsObj = map[string]any{"enabled": true, "server_name": serverName, "insecure": p.Insecure}
		if strings.TrimSpace(p.Fingerprint) != "" {
			tlsObj["utls"] = map[string]any{"enabled": true, "fingerprint": p.Fingerprint}
		}
		if strings.TrimSpace(p.ALPN) != "" {
			var alpn []string
			for _, x := range strings.Split(p.ALPN, ",") {
				if x = strings.TrimSpace(x); x != "" {
					alpn = append(alpn, x)
				}
			}
			if len(alpn) > 0 {
				tlsObj["alpn"] = alpn
			}
		}
		if strings.TrimSpace(p.ECHQueryName) != "" || strings.TrimSpace(p.ECH) != "" {
			qn := p.ECHQueryName
			if qn == "" {
				qn = strings.TrimSpace(strings.SplitN(p.ECH, "+", 2)[0])
			}
			tlsObj["ech"] = map[string]any{"enabled": true, "query_server_name": qn}
		}
	}
	transport := map[string]any{"type": "ws", "path": firstNonEmpty(p.Path, "/")}
	if strings.TrimSpace(p.Host) != "" {
		transport["headers"] = map[string]string{"Host": p.Host}
	}
	out := map[string]any{
		"type": "vless", "tag": tag, "server": strings.Trim(candidate, "[]"),
		"server_port": p.Port, "uuid": p.UUID, "network": "tcp", "transport": transport,
	}
	if strings.TrimSpace(p.Flow) != "" {
		out["flow"] = p.Flow
	}
	if len(tlsObj) > 0 {
		out["tls"] = tlsObj
	}
	return out, nil
}

func attachDNSConfig(cfg map[string]any, p config.RealProfile) {
	if strings.TrimSpace(p.ECHDoH) == "" {
		return
	}
	doh, err := url.Parse(p.ECHDoH)
	if err != nil || doh.Hostname() == "" {
		return
	}
	dohPort := 443
	if x, _ := strconv.Atoi(doh.Port()); x > 0 {
		dohPort = x
	}
	cfg["dns"] = map[string]any{
		"servers": []any{
			map[string]any{"type": "local", "tag": "local"},
			map[string]any{
				"type": "https", "tag": "ech-doh", "server": doh.Hostname(), "server_port": dohPort,
				"path":            firstNonEmpty(doh.EscapedPath(), "/dns-query"),
				"tls":             map[string]any{"enabled": true, "server_name": doh.Hostname()},
				"domain_resolver": "local",
			},
		},
		"final": "ech-doh",
	}
}

func freePorts(n int) ([]int, error) {
	if n < 1 {
		return nil, nil
	}
	listeners := make([]net.Listener, 0, n)
	ports := make([]int, 0, n)
	for i := 0; i < n; i++ {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			for _, x := range listeners {
				x.Close()
			}
			return nil, err
		}
		listeners = append(listeners, l)
		ports = append(ports, l.Addr().(*net.TCPAddr).Port)
	}
	for _, l := range listeners {
		l.Close()
	}
	return ports, nil
}

func waitPortsReady(ctx context.Context, ports []int, waitCh <-chan error, logs *lockedBuffer, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	pending := make(map[int]bool, len(ports))
	for _, p := range ports {
		pending[p] = true
	}
	for len(pending) > 0 {
		select {
		case err := <-waitCh:
			if err == nil {
				err = errors.New("sing-box 提前退出")
			}
			return fmt.Errorf("%v: %s", err, logs.Tail())
		default:
		}
		for p := range pending {
			conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p)), 100*time.Millisecond)
			if err == nil {
				conn.Close()
				delete(pending, p)
			}
		}
		if len(pending) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("sing-box 批量本地代理启动超时，未就绪=%d: %s", len(pending), logs.Tail())
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(60 * time.Millisecond):
		}
	}
	return nil
}

func BuildConfig(p config.RealProfile, candidate string, localPort int) (map[string]any, error) {
	if err := ValidateProfile(p); err != nil {
		return nil, err
	}
	out, err := buildOutbound(p, candidate, "test-out")
	if err != nil {
		return nil, err
	}
	cfg := map[string]any{
		"log": map[string]any{"level": "warn", "timestamp": false},
		"inbounds": []any{map[string]any{
			"type": "mixed", "tag": "mixed-in", "listen": "127.0.0.1", "listen_port": localPort,
		}},
		"outbounds": []any{out},
		"route":     map[string]any{"final": "test-out", "auto_detect_interface": true},
	}
	attachDNSConfig(cfg, p)
	return cfg, nil
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *lockedBuffer) Tail() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := strings.TrimSpace(b.b.String())
	if len([]rune(s)) > 500 {
		r := []rune(s)
		s = string(r[len(r)-500:])
	}
	return s
}
