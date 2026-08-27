package engine

import (
	"bufio"
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yourname/bestip-manager/internal/config"
)

type Result struct {
	IP        string    `json:"ip"`
	Family    string    `json:"family"`
	Colo      string    `json:"colo,omitempty"`
	LatencyMS float64   `json:"latency_ms,omitempty"`
	Loss      float64   `json:"loss,omitempty"`
	SpeedMB   float64   `json:"speed_mb,omitempty"`
	TestedAt  time.Time `json:"tested_at"`
}

type SourceStatus struct {
	SourceID  string    `json:"source_id"`
	Running   bool      `json:"running"`
	Stage     string    `json:"stage"`
	Error     string    `json:"error,omitempty"`
	StartedAt time.Time `json:"started_at,omitempty"`
	EndedAt   time.Time `json:"ended_at,omitempty"`
	Results   []Result  `json:"results"`
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
	defer func() { m.runMu.Lock(); delete(m.running, s.ID); m.runMu.Unlock() }()

	m.setStatus(SourceStatus{SourceID: s.ID, Running: true, Stage: "collecting inputs", StartedAt: time.Now()})
	workdir, err := os.MkdirTemp("", "bestip-"+sanitize(s.ID)+"-")
	if err != nil {
		return m.fail(s.ID, err)
	}
	defer os.RemoveAll(workdir)

	inputFile := filepath.Join(workdir, "ips.txt")
	if err := collectInputs(ctx, s, inputFile); err != nil {
		return m.fail(s.ID, err)
	}
	outFile := filepath.Join(workdir, "result.csv")

	m.patchStage(s.ID, "running cfst")
	bin := s.CFST.Binary
	if bin == "" {
		bin = "cfst"
	}
	args := buildArgs(s, inputFile, outFile)
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = workdir
	if out, err := cmd.CombinedOutput(); err != nil {
		return m.fail(s.ID, fmt.Errorf("cfst failed: %w: %s", err, tail(string(out), 1800)))
	}

	m.patchStage(s.ID, "parsing results")
	results, err := parseCSV(outFile, s.Family)
	if err != nil {
		return m.fail(s.ID, err)
	}
	keep := s.KeepResults
	if keep <= 0 {
		keep = 50
	}
	if len(results) > keep {
		results = results[:keep]
	}

	st := SourceStatus{SourceID: s.ID, Running: false, Stage: "done", StartedAt: m.getStartedAt(s.ID), EndedAt: time.Now(), Results: results}
	m.setStatus(st)
	return nil
}

func (m *Manager) getStartedAt(id string) time.Time {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.status[id].StartedAt
}
func (m *Manager) setStatus(st SourceStatus) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.status[st.SourceID] = st
}
func (m *Manager) patchStage(id, stage string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.status[id]
	st.Stage = stage
	m.status[id] = st
}
func (m *Manager) fail(id string, err error) error {
	m.mu.Lock()
	st := m.status[id]
	st.Running = false
	st.Stage = "failed"
	st.Error = err.Error()
	st.EndedAt = time.Now()
	m.status[id] = st
	m.mu.Unlock()
	return err
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
	if c.DownloadCount > 0 {
		args = append(args, "-dn", strconv.Itoa(c.DownloadCount))
	}
	if c.DownloadTime > 0 {
		args = append(args, "-dt", strconv.Itoa(c.DownloadTime))
	}
	if c.Port > 0 {
		args = append(args, "-tp", strconv.Itoa(c.Port))
	}
	if c.LatencyMaxMS > 0 {
		args = append(args, "-tl", fmt.Sprintf("%.2f", c.LatencyMaxMS))
	}
	if c.LatencyMinMS > 0 {
		args = append(args, "-tll", fmt.Sprintf("%.2f", c.LatencyMinMS))
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
	if c.HTTPing {
		args = append(args, "-httping")
	}
	if len(c.Colo) > 0 {
		args = append(args, "-cfcolo", strings.Join(c.Colo, ","))
	}
	if c.AllIP {
		args = append(args, "-allip")
	}
	return args
}

func collectInputs(ctx context.Context, s config.Source, outPath string) error {
	f, err := os.Create(outPath)
	if err != nil {
		return err
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
				return err
			}
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				resp.Body.Close()
				return fmt.Errorf("input %s returned %s", in, resp.Status)
			}
			r = resp.Body
		} else if st, err := os.Stat(in); err == nil && !st.IsDir() {
			rf, err := os.Open(in)
			if err != nil {
				return err
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
			return err
		}
	}
	if len(seen) == 0 {
		return fmt.Errorf("source %s produced no valid %s inputs", s.ID, s.Family)
	}
	return nil
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
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
