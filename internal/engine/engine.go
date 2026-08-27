package engine

import (
	"bufio"
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/honghanh691913-glitch/Cloudflare-BestIP/internal/config"
)

type Result struct {
	IP           string    `json:"ip"`
	Family       string    `json:"family"`
	Colo         string    `json:"colo,omitempty"`
	LatencyMS    float64   `json:"latency_ms,omitempty"`
	Loss         float64   `json:"loss,omitempty"`
	SpeedMB      float64   `json:"speed_mb,omitempty"`
	Qualified    bool      `json:"qualified"`
	RejectReason string    `json:"reject_reason,omitempty"`
	TestedAt     time.Time `json:"tested_at"`
}

type ScanProgress struct {
	Phase     string `json:"phase,omitempty"`
	Current   int    `json:"current"`
	Total     int    `json:"total"`
	Available int    `json:"available"`
	Percent   int    `json:"percent"`
}

type SourceStatus struct {
	SourceID      string       `json:"source_id"`
	Running       bool         `json:"running"`
	Stage         string       `json:"stage"`
	Error         string       `json:"error,omitempty"`
	StartedAt     time.Time    `json:"started_at,omitempty"`
	EndedAt       time.Time    `json:"ended_at,omitempty"`
	LastUpdate    time.Time    `json:"last_update,omitempty"`
	InputItems    int          `json:"input_items"`
	ObservedTotal int          `json:"observed_total"`
	Progress      ScanProgress `json:"progress"`
	Results       []Result     `json:"results"`
	Observed      []Result     `json:"observed"`
	Logs          []string     `json:"logs"`
}

type Manager struct {
	mu      sync.RWMutex
	status  map[string]SourceStatus
	runMu   sync.Mutex
	running map[string]bool
}

func NewManager() *Manager {
	return &Manager{status: map[string]SourceStatus{}, running: map[string]bool{}}
}

func (m *Manager) Snapshot() map[string]SourceStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := map[string]SourceStatus{}
	for k, v := range m.status {
		out[k] = v
	}
	return out
}

func (m *Manager) Latest(sourceID string) []Result {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]Result(nil), m.status[sourceID].Results...)
}

func (m *Manager) RunSource(ctx context.Context, s config.Source) error {
	m.runMu.Lock()
	if m.running[s.ID] {
		m.runMu.Unlock()
		return fmt.Errorf("source %s already running", s.ID)
	}
	m.running[s.ID] = true
	m.runMu.Unlock()
	defer func() {
		m.runMu.Lock()
		delete(m.running, s.ID)
		m.runMu.Unlock()
	}()

	prev := m.currentStatus(s.ID)
	started := time.Now()
	m.setStatus(SourceStatus{
		SourceID:   s.ID,
		Running:    true,
		Stage:      "准备扫描",
		StartedAt:  started,
		LastUpdate: started,
		Results:    append([]Result(nil), prev.Results...),
		Observed:   []Result{},
		Logs:       []string{},
	})
	m.logf(s.ID, "START name=%q family=%s inputs=%d all_ip=%v colo=%s thresholds(latency<=%.0fms loss<=%.2f speed>=%.2fMB/s)",
		s.Name, s.Family, len(s.Inputs), s.CFST.AllIP, strings.Join(s.CFST.Colo, ","),
		s.CFST.LatencyMaxMS, s.CFST.LossMax, s.CFST.SpeedMinMB)

	workdir, err := os.MkdirTemp("", "bestip-"+sanitize(s.ID)+"-")
	if err != nil {
		return m.fail(s.ID, err)
	}
	defer os.RemoveAll(workdir)

	inputFile := filepath.Join(workdir, "ips.txt")
	m.patchStage(s.ID, "读取 IP 源")
	inputItems, err := collectInputs(ctx, s, inputFile)
	if err != nil {
		return m.fail(s.ID, err)
	}
	m.patchInputItems(s.ID, inputItems)
	m.logf(s.ID, "INPUT ready items=%d file=%s", inputItems, inputFile)

	bin := s.CFST.Binary
	if bin == "" {
		bin = "cfst"
	}

	// Lightweight pre-scan: collect responsive IPs with relaxed delay/loss filters.
	// This makes the Web UI useful even if the final speed/region filter returns 0.
	probeFile := filepath.Join(workdir, "probe.csv")
	mainInputFile := inputFile
	m.patchStage(s.ID, "连通性预扫描")
	probeArgs := buildProbeArgs(s, inputFile, probeFile)
	if err := m.runCFST(ctx, s.ID, "probe", bin, probeArgs, workdir); err != nil {
		m.logf(s.ID, "WARN pre-scan failed: %v; continuing with main scan", err)
	} else if _, statErr := os.Stat(probeFile); statErr == nil {
		if observed, parseErr := parseCSV(probeFile, s.Family); parseErr == nil {
			observed = annotatePreScan(observed, s)
			m.setObserved(s.ID, observed)
			if len(observed) > 0 {
				exactInput := filepath.Join(workdir, "responsive-ips.txt")
				if writeResultIPs(exactInput, observed) == nil {
					mainInputFile = exactInput
				}
			}
			m.logf(s.ID, "PROBE done responsive=%d; main scan will reuse the same responsive IP set", len(observed))
		} else {
			m.logf(s.ID, "WARN unable to parse pre-scan CSV: %v", parseErr)
		}
	} else {
		m.logf(s.ID, "PROBE done responsive=0 (CFST created no CSV)")
	}

	outFile := filepath.Join(workdir, "result.csv")
	m.patchStage(s.ID, "正式测速")
	args := buildArgs(s, mainInputFile, outFile)
	if err := m.runCFST(ctx, s.ID, "main", bin, args, workdir); err != nil {
		return m.fail(s.ID, err)
	}

	m.patchStage(s.ID, "解析结果")
	if _, statErr := os.Stat(outFile); statErr != nil {
		if os.IsNotExist(statErr) {
			m.finalizeObserved(s.ID, nil, s)
			return m.fail(s.ID, fmt.Errorf(
				"本次正式测速完成，但 0 个 IP 进入最终结果；扫描详情里仍可查看预扫描 IP、延迟和丢包",
			))
		}
		return m.fail(s.ID, statErr)
	}

	rawResults, err := parseCSV(outFile, s.Family)
	if err != nil {
		return m.fail(s.ID, err)
	}
	for i := range rawResults {
		rawResults[i].Qualified = true
		rawResults[i].RejectReason = ""
	}

	finalResults := rawResults
	if len(s.CFST.Colo) > 0 {
		finalResults = filterResultsByColo(rawResults, s.CFST.Colo)
	}
	for i := range finalResults {
		finalResults[i].Qualified = true
		finalResults[i].RejectReason = ""
	}

	m.finalizeObserved(s.ID, rawResults, s)

	if len(finalResults) == 0 {
		return m.fail(s.ID, fmt.Errorf(
			"测速已有速度结果，但没有结果匹配地区 %s；请在扫描详情查看各 IP 实际 Colo",
			strings.Join(s.CFST.Colo, ","),
		))
	}

	keep := s.KeepResults
	if keep <= 0 {
		keep = 50
	}
	if len(finalResults) > keep {
		finalResults = finalResults[:keep]
	}

	st := m.currentStatus(s.ID)
	st.Running = false
	st.Stage = "完成"
	st.Error = ""
	st.EndedAt = time.Now()
	st.LastUpdate = time.Now()
	st.Results = finalResults
	st.Progress = ScanProgress{Phase: "done", Current: len(finalResults), Total: len(finalResults), Available: len(finalResults), Percent: 100}
	m.setStatus(st)
	m.logf(s.ID, "DONE qualified=%d observed=%d elapsed=%s", len(finalResults), st.ObservedTotal, time.Since(started).Round(time.Millisecond))
	return nil
}

func (m *Manager) runCFST(ctx context.Context, sourceID, phase, bin string, args []string, workdir string) error {
	m.logf(sourceID, "CFST %s command: %s %s", strings.ToUpper(phase), bin, strings.Join(args, " "))
	stream := newCFSTStream(m, sourceID, phase)
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = workdir
	cmd.Stdout = stream
	cmd.Stderr = stream
	if err := cmd.Run(); err != nil {
		msg := summarizeCFSTError(stream.Tail())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("CFST %s 失败：%s", phase, msg)
	}
	stream.Flush()
	return nil
}

func annotatePreScan(results []Result, s config.Source) []Result {
	for i := range results {
		r := &results[i]
		r.Qualified = false
		reasons := preliminaryRejectReasons(*r, s)
		if len(reasons) == 0 {
			r.RejectReason = "等待速度/地区确认"
		} else {
			r.RejectReason = strings.Join(reasons, "；")
		}
	}
	return results
}

func preliminaryRejectReasons(r Result, s config.Source) []string {
	reasons := []string{}
	if s.CFST.LatencyMaxMS > 0 && r.LatencyMS > s.CFST.LatencyMaxMS {
		reasons = append(reasons, fmt.Sprintf("延迟 %.1fms > %.0fms", r.LatencyMS, s.CFST.LatencyMaxMS))
	}
	if s.CFST.LatencyMinMS > 0 && r.LatencyMS < s.CFST.LatencyMinMS {
		reasons = append(reasons, fmt.Sprintf("延迟 %.1fms < %.0fms", r.LatencyMS, s.CFST.LatencyMinMS))
	}
	if s.CFST.LossMax >= 0 && r.Loss > s.CFST.LossMax {
		reasons = append(reasons, fmt.Sprintf("丢包 %.0f%% > %.0f%%", r.Loss*100, s.CFST.LossMax*100))
	}
	return reasons
}

func (m *Manager) finalizeObserved(id string, raw []Result, s config.Source) {
	st := m.currentStatus(id)
	byIP := map[string]Result{}
	for _, r := range st.Observed {
		byIP[r.IP] = r
	}

	wanted := map[string]bool{}
	for _, c := range s.CFST.Colo {
		wanted[strings.ToUpper(strings.TrimSpace(c))] = true
	}
	for _, r := range raw {
		r.Qualified = true
		r.RejectReason = ""
		if len(wanted) > 0 && !wanted[strings.ToUpper(strings.TrimSpace(r.Colo))] {
			r.Qualified = false
			if strings.TrimSpace(r.Colo) == "" {
				r.RejectReason = "未取得 Colo，未通过地区筛选"
			} else {
				r.RejectReason = fmt.Sprintf("地区 %s 不匹配 %s", r.Colo, strings.Join(s.CFST.Colo, ","))
			}
		}
		byIP[r.IP] = r
	}

	observed := make([]Result, 0, len(byIP))
	for _, old := range st.Observed {
		if r, ok := byIP[old.IP]; ok {
			if r.SpeedMB == 0 && !r.Qualified && r.RejectReason == "等待速度/地区确认" {
				r.RejectReason = "未进入最终下载结果（可能速度不足、下载失败或未进入下载队列）"
			}
			observed = append(observed, r)
			delete(byIP, old.IP)
		}
	}
	for _, r := range byIP {
		observed = append(observed, r)
	}
	m.setObserved(id, observed)
}

func (m *Manager) getStartedAt(id string) time.Time {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.status[id].StartedAt
}

func (m *Manager) currentStatus(id string) SourceStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.status[id]
}

func (m *Manager) setStatus(st SourceStatus) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if st.LastUpdate.IsZero() {
		st.LastUpdate = time.Now()
	}
	m.status[st.SourceID] = st
}

func (m *Manager) patchStage(id, stage string) {
	m.mu.Lock()
	st := m.status[id]
	st.Stage = stage
	st.LastUpdate = time.Now()
	m.status[id] = st
	m.mu.Unlock()
	m.logf(id, "STAGE %s", stage)
}

func (m *Manager) patchInputItems(id string, n int) {
	m.mu.Lock()
	st := m.status[id]
	st.InputItems = n
	st.LastUpdate = time.Now()
	m.status[id] = st
	m.mu.Unlock()
}

func (m *Manager) patchProgress(id string, p ScanProgress) {
	if p.Total > 0 {
		p.Percent = p.Current * 100 / p.Total
		if p.Percent > 100 {
			p.Percent = 100
		}
	}
	m.mu.Lock()
	st := m.status[id]
	st.Progress = p
	st.LastUpdate = time.Now()
	m.status[id] = st
	m.mu.Unlock()
}

func (m *Manager) setObserved(id string, rows []Result) {
	const maxRows = 500
	total := len(rows)
	if len(rows) > maxRows {
		rows = rows[:maxRows]
	}
	m.mu.Lock()
	st := m.status[id]
	st.ObservedTotal = total
	st.Observed = append([]Result(nil), rows...)
	st.LastUpdate = time.Now()
	m.status[id] = st
	m.mu.Unlock()
}

func (m *Manager) addLog(id, line string) {
	line = strings.TrimSpace(stripANSI(line))
	if line == "" {
		return
	}
	if len([]rune(line)) > 360 {
		r := []rune(line)
		line = string(r[:360]) + "…"
	}
	entry := time.Now().Format("15:04:05") + " " + line
	m.mu.Lock()
	st := m.status[id]
	st.Logs = append(st.Logs, entry)
	if len(st.Logs) > 100 {
		st.Logs = append([]string(nil), st.Logs[len(st.Logs)-100:]...)
	}
	st.LastUpdate = time.Now()
	m.status[id] = st
	m.mu.Unlock()
}

func (m *Manager) logf(id, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	log.Printf("[scan:%s] %s", id, msg)
	m.addLog(id, msg)
}

func (m *Manager) fail(id string, err error) error {
	m.mu.Lock()
	st := m.status[id]
	st.Running = false
	st.Stage = "失败"
	st.Error = err.Error()
	st.EndedAt = time.Now()
	st.LastUpdate = time.Now()
	m.status[id] = st
	m.mu.Unlock()
	m.logf(id, "FAILED: %v", err)
	return err
}

var (
	progressRE  = regexp.MustCompile(`(\d+)\s*/\s*(\d+)`)
	availableRE = regexp.MustCompile(`可用:\s*(\d+)`)
	ansiRE      = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)
)

type cfstStream struct {
	m         *Manager
	sourceID  string
	phase     string
	mu        sync.Mutex
	buffer    string
	tail      []string
	lastPct   int
	lastStage string
}

func newCFSTStream(m *Manager, sourceID, phase string) *cfstStream {
	return &cfstStream{m: m, sourceID: sourceID, phase: phase, lastPct: -10}
}

func (w *cfstStream) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buffer += string(p)
	for {
		idx := strings.IndexAny(w.buffer, "\r\n")
		if idx < 0 {
			break
		}
		part := w.buffer[:idx]
		w.buffer = w.buffer[idx+1:]
		w.consume(part)
	}
	// Progress bars may rewrite the same line without a final newline.
	if len(w.buffer) > 0 && progressRE.MatchString(stripANSI(w.buffer)) {
		w.consume(w.buffer)
		w.buffer = ""
	}
	return len(p), nil
}

func (w *cfstStream) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if strings.TrimSpace(w.buffer) != "" {
		w.consume(w.buffer)
		w.buffer = ""
	}
}

func (w *cfstStream) Tail() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return strings.Join(w.tail, "\n")
}

func (w *cfstStream) consume(raw string) {
	line := strings.TrimSpace(stripANSI(raw))
	if line == "" {
		return
	}
	w.tail = append(w.tail, line)
	if len(w.tail) > 30 {
		w.tail = append([]string(nil), w.tail[len(w.tail)-30:]...)
	}

	stage := ""
	switch {
	case strings.Contains(line, "开始延迟测速"):
		stage = "延迟测速"
	case strings.Contains(line, "开始下载测速"):
		stage = "下载测速"
	case strings.Contains(line, "完整测速结果"):
		stage = "整理结果"
	}
	if stage != "" && stage != w.lastStage {
		w.lastStage = stage
		w.m.patchStage(w.sourceID, stage)
	}

	if m := progressRE.FindStringSubmatch(line); len(m) == 3 {
		cur, _ := strconv.Atoi(m[1])
		total, _ := strconv.Atoi(m[2])
		available := w.m.currentStatus(w.sourceID).Progress.Available
		if a := availableRE.FindStringSubmatch(line); len(a) == 2 {
			available, _ = strconv.Atoi(a[1])
		}
		p := ScanProgress{Phase: w.phase, Current: cur, Total: total, Available: available}
		if stage != "" {
			p.Phase = stage
		}
		w.m.patchProgress(w.sourceID, p)
		pct := 0
		if total > 0 {
			pct = cur * 100 / total
		}
		if pct >= w.lastPct+10 || pct == 100 {
			w.lastPct = pct
			w.m.logf(w.sourceID, "%s progress %d/%d (%d%%) available=%d", strings.ToUpper(w.phase), cur, total, pct, available)
		}
		return
	}

	// Keep useful CFST lines in both Docker logs and the Web detail panel.
	if strings.Contains(line, "CloudflareSpeedTest") ||
		strings.Contains(line, "开始") ||
		strings.Contains(line, "[信息]") ||
		strings.Contains(line, "[提示]") ||
		strings.Contains(line, "[调试]") ||
		strings.Contains(line, "错误") ||
		strings.Contains(strings.ToLower(line), "error") {
		w.m.logf(w.sourceID, "CFST/%s %s", w.phase, line)
	}
}

func stripANSI(s string) string {
	return ansiRE.ReplaceAllString(s, "")
}

func buildProbeArgs(s config.Source, inputFile, outFile string) []string {
	c := s.CFST
	args := []string{"-f", inputFile, "-o", outFile, "-dd", "-p", "0"}
	if c.Threads > 0 {
		args = append(args, "-t", strconv.Itoa(c.Threads))
	}
	if c.PingCount > 0 {
		args = append(args, "-n", strconv.Itoa(c.PingCount))
	}
	if c.Port > 0 {
		args = append(args, "-tp", strconv.Itoa(c.Port))
	}
	if c.URL != "" && c.HTTPing && len(c.Colo) == 0 {
		args = append(args, "-url", c.URL)
	}
	if c.HTTPing && len(c.Colo) == 0 {
		args = append(args, "-httping")
	}
	if c.AllIP {
		args = append(args, "-allip")
	}
	// Deliberately omit -tl/-tll/-tlr/-sl/-cfcolo so the pre-scan preserves
	// responsive IPs that later fail user thresholds.
	return args
}

func buildArgs(s config.Source, inputFile, outFile string) []string {
	c := s.CFST
	args := []string{"-f", inputFile, "-o", outFile}
	if c.Threads > 0 {
		args = append(args, "-t", strconv.Itoa(c.Threads))
	}
	if c.PingCount > 0 {
		args = append(args, "-n", strconv.Itoa(c.PingCount))
	}
	downloadCount := c.DownloadCount
	if downloadCount <= 0 {
		downloadCount = 10
	}
	if len(c.Colo) > 0 {
		want := s.KeepResults
		if want <= 0 {
			want = 50
		}
		if downloadCount < want {
			downloadCount = want
		}
	}
	args = append(args, "-dn", strconv.Itoa(downloadCount))
	if c.DownloadTime > 0 {
		args = append(args, "-dt", strconv.Itoa(c.DownloadTime))
	}
	if c.Port > 0 {
		args = append(args, "-tp", strconv.Itoa(c.Port))
	}
	// CFST v2.3.x defines -tl / -tll as integer flags.
	// Passing values such as 200.00 makes flag parsing fail with exit status 2.
	if c.LatencyMaxMS > 0 {
		args = append(args, "-tl", strconv.Itoa(int(c.LatencyMaxMS+0.5)))
	}
	if c.LatencyMinMS > 0 {
		args = append(args, "-tll", strconv.Itoa(int(c.LatencyMinMS+0.5)))
	}
	if c.LossMax > 0 {
		args = append(args, "-tlr", fmt.Sprintf("%.4f", c.LossMax))
	}
	if c.SpeedMinMB > 0 {
		args = append(args, "-sl", fmt.Sprintf("%.2f", c.SpeedMinMB))
	}
	if c.URL != "" {
		args = append(args, "-url", c.URL)
	}
	// With a region filter, use TCPing + download and filter the CSV's actual
	// Colo afterwards. This avoids losing candidates during HTTP HEAD pre-filtering.
	if c.HTTPing && len(c.Colo) == 0 {
		args = append(args, "-httping")
	}
	// We consume result.csv ourselves; suppress the verbose terminal result table.
	args = append(args, "-p", "0")
	if c.AllIP {
		args = append(args, "-allip")
	}
	return args
}

func collectInputs(ctx context.Context, s config.Source, outPath string) (int, error) {
	f, err := os.Create(outPath)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	defer w.Flush()
	seen := map[string]bool{}
	for _, in := range s.Inputs {
		in = strings.TrimSpace(in)
		if in == "" {
			continue
		}
		var r io.ReadCloser
		if strings.HasPrefix(in, "http://") || strings.HasPrefix(in, "https://") {
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, in, nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return 0, err
			}
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				resp.Body.Close()
				return 0, fmt.Errorf("input %s returned %s", in, resp.Status)
			}
			r = resp.Body
		} else if st, err := os.Stat(in); err == nil && !st.IsDir() {
			rf, err := os.Open(in)
			if err != nil {
				return 0, err
			}
			r = rf
		} else {
			lines := []string{in}
			for _, line := range lines {
				if validForFamily(line, s.Family) && !seen[line] {
					seen[line] = true
					fmt.Fprintln(w, line)
				}
			}
			continue
		}
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			line := strings.TrimSpace(strings.Split(sc.Text(), "#")[0])
			if line == "" {
				continue
			}
			if validForFamily(line, s.Family) && !seen[line] {
				seen[line] = true
				fmt.Fprintln(w, line)
			}
		}
		err = sc.Err()
		r.Close()
		if err != nil {
			return 0, err
		}
	}
	if len(seen) == 0 {
		return 0, fmt.Errorf("source %s produced no valid %s inputs", s.ID, s.Family)
	}
	return len(seen), nil
}

func validForFamily(v, family string) bool {
	base := v
	if i := strings.Index(v, "/"); i >= 0 {
		base = v[:i]
	}
	ip := net.ParseIP(strings.TrimSpace(base))
	if ip == nil {
		return false
	}
	if family == "ipv4" {
		return ip.To4() != nil
	}
	return ip.To4() == nil
}

func writeResultIPs(path string, rows []Result) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	defer w.Flush()
	for _, r := range rows {
		if net.ParseIP(strings.TrimSpace(r.IP)) != nil {
			if _, err := fmt.Fprintln(w, strings.TrimSpace(r.IP)); err != nil {
				return err
			}
		}
	}
	return nil
}

func filterResultsByColo(results []Result, wanted []string) []Result {
	if len(wanted) == 0 {
		return results
	}
	allowed := map[string]struct{}{}
	for _, v := range wanted {
		v = strings.ToUpper(strings.TrimSpace(v))
		if v != "" {
			allowed[v] = struct{}{}
		}
	}
	out := make([]Result, 0, len(results))
	for _, r := range results {
		colo := strings.ToUpper(strings.TrimSpace(r.Colo))
		if _, ok := allowed[colo]; ok {
			out = append(out, r)
		}
	}
	return out
}

func parseCSV(path, family string) ([]Result, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	rows, err := r.ReadAll()
	if err != nil {
		return nil, err
	}
	if len(rows) < 2 {
		return nil, fmt.Errorf("cfst returned no usable rows")
	}
	header := rows[0]
	idxIP, idxLatency, idxSpeed, idxLoss, idxColo := 0, -1, -1, -1, -1
	for i, h := range header {
		x := strings.ToLower(strings.TrimSpace(h))
		switch {
		case strings.Contains(x, "ip"):
			idxIP = i
		case strings.Contains(x, "延迟") || strings.Contains(x, "latency"):
			idxLatency = i
		case strings.Contains(x, "速度") || strings.Contains(x, "speed"):
			idxSpeed = i
		case strings.Contains(x, "丢包") || strings.Contains(x, "loss"):
			idxLoss = i
		case strings.Contains(x, "地区") || strings.Contains(x, "colo") || strings.Contains(x, "region"):
			idxColo = i
		}
	}
	out := []Result{}
	for _, row := range rows[1:] {
		if idxIP >= len(row) {
			continue
		}
		ip := strings.TrimSpace(row[idxIP])
		if net.ParseIP(ip) == nil {
			continue
		}
		rr := Result{IP: ip, Family: family, TestedAt: time.Now()}
		if idxLatency >= 0 && idxLatency < len(row) {
			rr.LatencyMS = parseNumber(row[idxLatency])
		}
		if idxSpeed >= 0 && idxSpeed < len(row) {
			rr.SpeedMB = parseNumber(row[idxSpeed])
		}
		if idxLoss >= 0 && idxLoss < len(row) {
			rr.Loss = parsePercent(row[idxLoss])
		}
		if idxColo >= 0 && idxColo < len(row) {
			rr.Colo = strings.TrimSpace(row[idxColo])
		}
		out = append(out, rr)
	}
	return out, nil
}
func parseNumber(s string) float64 {
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, "ms")
	s = strings.TrimSuffix(s, "MB/s")
	s = strings.TrimSpace(s)
	v, _ := strconv.ParseFloat(s, 64)
	return v
}
func parsePercent(s string) float64 {
	s = strings.TrimSpace(s)
	if strings.HasSuffix(s, "%") {
		v, _ := strconv.ParseFloat(strings.TrimSuffix(s, "%"), 64)
		return v / 100
	}
	v, _ := strconv.ParseFloat(s, 64)
	return v
}
func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, s)
}
func summarizeCFSTError(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	needles := []string{
		"invalid value",
		"flag provided but not defined",
		"invalid argument",
		"parse error",
		"no such file",
		"permission denied",
	}
	for _, line := range lines {
		t := strings.TrimSpace(line)
		lower := strings.ToLower(t)
		for _, needle := range needles {
			if strings.Contains(lower, needle) {
				if len([]rune(t)) > 260 {
					r := []rune(t)
					return string(r[:260]) + "…"
				}
				return t
			}
		}
	}
	for _, line := range lines {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "参数：") || strings.HasPrefix(t, "Usage of") || strings.HasPrefix(t, "CloudflareSpeedTest ") {
			continue
		}
		if len([]rune(t)) > 260 {
			r := []rune(t)
			return string(r[:260]) + "…"
		}
		return t
	}
	return ""
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
