package engine

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
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
	SpeedTested  bool      `json:"speed_tested"`
	Qualified    bool      `json:"qualified"`
	RejectReason string    `json:"reject_reason,omitempty"`
	TestedAt     time.Time `json:"tested_at"`
}

type ScanProgress struct {
	Phase          string    `json:"phase,omitempty"`
	Current        int       `json:"current"`
	Total          int       `json:"total"`
	Available      int       `json:"available"`
	Percent        int       `json:"percent"`
	StartedAt      time.Time `json:"started_at,omitempty"`
	ElapsedSeconds int       `json:"elapsed_seconds,omitempty"`
	ETASeconds     int       `json:"eta_seconds,omitempty"`
}

type FunnelStats struct {
	TotalCandidates int `json:"total_candidates"`
	Responsive      int `json:"responsive"`
	LatencyPassed   int `json:"latency_passed"`
	LossPassed      int `json:"loss_passed"`
	RegionPassed    int `json:"region_passed"`
	SpeedTested     int `json:"speed_tested"`
	SpeedPassed     int `json:"speed_passed"`
}

type SourceStatus struct {
	SourceID       string       `json:"source_id"`
	Running        bool         `json:"running"`
	Stage          string       `json:"stage"`
	Error          string       `json:"error,omitempty"`
	StartedAt      time.Time    `json:"started_at,omitempty"`
	EndedAt        time.Time    `json:"ended_at,omitempty"`
	LastUpdate     time.Time    `json:"last_update,omitempty"`
	InputItems     int          `json:"input_items"`
	CandidateCount int          `json:"candidate_count"`
	ObservedTotal  int          `json:"observed_total"`
	Progress       ScanProgress `json:"progress"`
	Funnel         FunnelStats  `json:"funnel"`
	Results        []Result     `json:"results"`
	Observed       []Result     `json:"observed"`
	Logs           []string     `json:"logs"`
}

type Manager struct {
	mu      sync.RWMutex
	status  map[string]SourceStatus
	history map[string][]Result
	runMu   sync.Mutex
	running map[string]bool
	cancels map[string]context.CancelFunc
}

func NewManager() *Manager {
	return &Manager{
		status: map[string]SourceStatus{},
		history: map[string][]Result{},
		running: map[string]bool{},
		cancels: map[string]context.CancelFunc{},
	}
}

func (m *Manager) beginRun(parent context.Context, sourceID string) (context.Context, bool) {
	m.runMu.Lock()
	defer m.runMu.Unlock()
	if m.running[sourceID] {
		return nil, false
	}
	ctx, cancel := context.WithCancel(parent)
	m.running[sourceID] = true
	m.cancels[sourceID] = cancel
	return ctx, true
}

func (m *Manager) endRun(sourceID string) {
	m.runMu.Lock()
	if cancel := m.cancels[sourceID]; cancel != nil {
		cancel()
	}
	delete(m.cancels, sourceID)
	delete(m.running, sourceID)
	m.runMu.Unlock()
}

func (m *Manager) StopSource(sourceID string) bool {
	m.runMu.Lock()
	cancel := m.cancels[sourceID]
	running := m.running[sourceID]
	m.runMu.Unlock()
	if !running || cancel == nil {
		return false
	}
	m.mu.Lock()
	st := m.status[sourceID]
	st.Stage = "正在停止"
	st.LastUpdate = time.Now()
	m.status[sourceID] = st
	m.mu.Unlock()
	m.logf(sourceID, "STOP requested by user")
	cancel()
	return true
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

// History returns the full lightweight observation set for the most recent run.
// It includes candidates eliminated before the speed final so the Furnace can
// learn that a previously-good IP later became slow, lossy, or unreachable.
func (m *Manager) History(sourceID string) []Result {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]Result(nil), m.history[sourceID]...)
}

func (m *Manager) IsRunning(sourceID string) bool {
	m.runMu.Lock()
	defer m.runMu.Unlock()
	return m.running[sourceID]
}

func (m *Manager) RunSource(ctx context.Context, s config.Source) error {
	var ok bool
	ctx, ok = m.beginRun(ctx, s.ID)
	if !ok {
		return fmt.Errorf("source %s already running", s.ID)
	}
	defer m.endRun(s.ID)

	prev := m.currentStatus(s.ID)
	started := time.Now()
	m.setStatus(SourceStatus{
		SourceID:   s.ID,
		Running:    true,
		Stage:      "准备严选",
		StartedAt:  started,
		LastUpdate: started,
		// DNS keeps using the previous complete result set until this run finishes.
		Results:  append([]Result(nil), prev.Results...),
		Observed: []Result{},
		Logs:     []string{},
	})
	m.logf(s.ID, "STRICT START name=%q family=%s inputs=%d sample=%d colo=%s thresholds(latency<=%.0fms loss<=%.2f speed>=%.2fMB/s)",
		s.Name, s.Family, len(s.Inputs), s.SampleCount, strings.Join(s.CFST.Colo, ","),
		s.CFST.LatencyMaxMS, s.CFST.LossMax, s.CFST.SpeedMinMB)

	workdir, err := os.MkdirTemp("", "bestip-"+sanitize(s.ID)+"-")
	if err != nil {
		return m.fail(s.ID, err)
	}
	defer os.RemoveAll(workdir)

	rawInputFile := filepath.Join(workdir, "ranges.txt")
	m.patchStage(s.ID, "读取 IP 源")
	inputItems, err := collectInputs(ctx, s, rawInputFile)
	if err != nil {
		return m.fail(s.ID, err)
	}
	m.patchInputItems(s.ID, inputItems)

	// v0.6 owns candidate generation instead of delegating CIDR expansion to CFST.
	// This makes IPv4 and IPv6 sampling predictable and allows an exact scan count.
	inputFile := filepath.Join(workdir, "candidates.txt")
	m.patchStage(s.ID, "生成候选 IP")
	candidateCount, allocations, err := sampleCandidateFile(rawInputFile, inputFile, s.Family, s.SampleCount, s.GlobalMaxSample)
	if err != nil {
		return m.fail(s.ID, err)
	}
	m.patchCandidateCount(s.ID, candidateCount)
	preHistory, err := readCandidateHistory(inputFile, s.Family)
	if err == nil {
		m.setHistory(s.ID, preHistory)
	}
	m.logf(s.ID, "INPUT ready ranges=%d candidates=%d allocation=%s", inputItems, candidateCount, strings.Join(allocations, " | "))

	bin := s.CFST.Binary
	if bin == "" {
		bin = "cfst"
	}

	// Round 1: fast, highly parallel latency/loss scan of the whole candidate set.
	// Download is disabled, so even an IPv4 /24 can be screened in a few seconds.
	probeFile := filepath.Join(workdir, "probe.csv")
	m.patchStage(s.ID, "延迟 / 丢包初筛")
	probeArgs := buildProbeArgs(s, inputFile, probeFile)
	if err := m.runCFST(ctx, s.ID, "probe", bin, probeArgs, workdir); err != nil {
		return m.fail(s.ID, err)
	}
	if _, statErr := os.Stat(probeFile); statErr != nil {
		if os.IsNotExist(statErr) {
			if s.Family == "ipv6" {
				if hint := diagnoseIPv6Egress(ctx); hint != "" {
					return m.fail(s.ID, fmt.Errorf("初筛完成，但没有任何可连通 IPv6；%s", hint))
				}
			}
			return m.fail(s.ID, fmt.Errorf("初筛完成，但没有任何可连通 IP"))
		}
		return m.fail(s.ID, statErr)
	}
	probeRows, err := parseCSV(probeFile, s.Family)
	if err != nil {
		return m.fail(s.ID, err)
	}

	st := m.currentStatus(s.ID)
	total := st.CandidateCount
	if total <= 0 {
		total = st.Progress.Total
	}
	if total < len(probeRows) {
		total = len(probeRows)
	}
	funnel := FunnelStats{TotalCandidates: total, Responsive: len(probeRows)}

	now := time.Now()
	historyRows := m.History(s.ID)
	for i := range historyRows {
		historyRows[i].TestedAt = now
		historyRows[i].Qualified = false
	}

	latencyRows := make([]Result, 0, len(probeRows))
	for _, r := range probeRows {
		r.TestedAt = now
		m.updateHistoryRowSlice(historyRows, r.IP, func(x *Result) {
			x.LatencyMS = r.LatencyMS
			x.Loss = r.Loss
			x.TestedAt = now
			x.RejectReason = "待延迟/丢包判定"
		})
		if s.CFST.LatencyMaxMS > 0 && r.LatencyMS > s.CFST.LatencyMaxMS {
			m.updateHistoryRowSlice(historyRows, r.IP, func(x *Result) {
				x.RejectReason = fmt.Sprintf("延迟 %.1fms > %.1fms", r.LatencyMS, s.CFST.LatencyMaxMS)
			})
			continue
		}
		if s.CFST.LatencyMinMS > 0 && r.LatencyMS < s.CFST.LatencyMinMS {
			m.updateHistoryRowSlice(historyRows, r.IP, func(x *Result) {
				x.RejectReason = fmt.Sprintf("延迟 %.1fms < %.1fms", r.LatencyMS, s.CFST.LatencyMinMS)
			})
			continue
		}
		latencyRows = append(latencyRows, r)
	}
	funnel.LatencyPassed = len(latencyRows)

	lossRows := make([]Result, 0, len(latencyRows))
	for _, r := range latencyRows {
		if s.CFST.LossMax >= 0 && r.Loss > s.CFST.LossMax {
			m.updateHistoryRowSlice(historyRows, r.IP, func(x *Result) {
				x.RejectReason = fmt.Sprintf("丢包 %.0f%% > %.0f%%", r.Loss*100, s.CFST.LossMax*100)
			})
			continue
		}
		lossRows = append(lossRows, r)
		m.updateHistoryRowSlice(historyRows, r.IP, func(x *Result) { x.RejectReason = "待地区/速度决赛" })
	}
	m.setHistory(s.ID, historyRows)
	funnel.LossPassed = len(lossRows)
	m.patchFunnel(s.ID, funnel)
	m.logf(s.ID, "FUNNEL initial total=%d responsive=%d latency_pass=%d loss_pass=%d",
		funnel.TotalCandidates, funnel.Responsive, funnel.LatencyPassed, funnel.LossPassed)

	if len(lossRows) == 0 {
		return m.fail(s.ID, fmt.Errorf("全部 IP 已在延迟/丢包初筛中淘汰"))
	}

	// Round 2: lightweight Colo lookup only for the latency/loss survivors.
	// This follows the same efficient idea as CFData-WEB: trace/Colo is resolved
	// before expensive bandwidth testing.
	finalists := lossRows
	if len(s.CFST.Colo) > 0 {
		m.patchStage(s.ID, "地区筛选")
	} else {
		m.patchStage(s.ID, "地区识别")
	}
	// Even when the task does not restrict Colo, resolve it for finalists as
	// lightweight informational metadata. This keeps the UI from showing N/A
	// and costs only one small trace request per finalist, in parallel.
	finalists = m.filterByRegion(ctx, s.ID, s, lossRows)
	funnel = m.currentStatus(s.ID).Funnel
	if len(finalists) == 0 {
		return m.fail(s.ID, fmt.Errorf("延迟/丢包已通过，但没有 IP 匹配地区 %s", strings.Join(s.CFST.Colo, ",")))
	}

	for i := range finalists {
		finalists[i].Qualified = false
		finalists[i].SpeedTested = false
		finalists[i].RejectReason = "待速度决赛"
	}
	sort.SliceStable(finalists, func(i, j int) bool { return finalists[i].LatencyMS < finalists[j].LatencyMS })
	m.setObserved(s.ID, finalists)
	m.logf(s.ID, "FINALS ready=%d; all finalists will be speed-tested for strict ranking", len(finalists))

	// Round 3: speed final. Every surviving IP is tested. Results appear live:
	// qualified rows are pinned to the top and sorted by measured speed.
	m.patchStage(s.ID, "速度决赛")
	for i := range finalists {
		if err := ctx.Err(); err != nil {
			return m.fail(s.ID, err)
		}
		speed, err := measureDownloadSpeed(ctx, s, finalists[i].IP)
		finalists[i].SpeedTested = true
		finalists[i].TestedAt = time.Now()

		if err != nil {
			finalists[i].Qualified = false
			finalists[i].RejectReason = "测速失败：" + compactReason(err.Error())
			m.logf(s.ID, "SPEED %d/%d ip=%s failed=%v", i+1, len(finalists), finalists[i].IP, err)
		} else {
			finalists[i].SpeedMB = speed
			if s.CFST.SpeedMinMB > 0 && speed < s.CFST.SpeedMinMB {
				finalists[i].Qualified = false
				finalists[i].RejectReason = fmt.Sprintf("速度 %.2f MB/s < %.2f MB/s", speed, s.CFST.SpeedMinMB)
				m.logf(s.ID, "SPEED %d/%d ip=%s speed=%.2fMB/s NOT_QUALIFIED", i+1, len(finalists), finalists[i].IP, speed)
			} else {
				finalists[i].Qualified = true
				finalists[i].RejectReason = ""
				m.logf(s.ID, "SPEED %d/%d ip=%s speed=%.2fMB/s QUALIFIED", i+1, len(finalists), finalists[i].IP, speed)
			}
		}

		m.updateHistoryRow(s.ID, finalists[i])

		funnel := m.currentStatus(s.ID).Funnel
		funnel.SpeedTested = i + 1
		funnel.SpeedPassed = countQualified(finalists)
		m.patchFunnel(s.ID, funnel)
		m.patchProgress(s.ID, ScanProgress{
			Phase:     "speed",
			Current:   i + 1,
			Total:     len(finalists),
			Available: funnel.SpeedPassed,
		})
		m.setObserved(s.ID, sortObservedForDisplay(finalists))
	}

	qualified := make([]Result, 0, len(finalists))
	for _, r := range finalists {
		if r.Qualified {
			qualified = append(qualified, r)
		}
	}
	sort.SliceStable(qualified, func(i, j int) bool {
		if qualified[i].SpeedMB == qualified[j].SpeedMB {
			return qualified[i].LatencyMS < qualified[j].LatencyMS
		}
		return qualified[i].SpeedMB > qualified[j].SpeedMB
	})

	if len(qualified) == 0 {
		return m.fail(s.ID, fmt.Errorf("速度决赛完成，但没有 IP 达到 %.2f MB/s 的速度及格线", s.CFST.SpeedMinMB))
	}

	keep := s.KeepResults
	if keep <= 0 {
		keep = 50
	}
	if len(qualified) > keep {
		qualified = qualified[:keep]
	}

	st = m.currentStatus(s.ID)
	st.Running = false
	st.Stage = "严选完成"
	st.Error = ""
	st.EndedAt = time.Now()
	st.LastUpdate = time.Now()
	st.Results = append([]Result(nil), qualified...)
	st.Observed = sortObservedForDisplay(finalists)
	st.ObservedTotal = len(finalists)
	st.Progress = ScanProgress{
		Phase:     "done",
		Current:   len(finalists),
		Total:     len(finalists),
		Available: len(qualified),
		Percent:   100,
	}
	st.Funnel.SpeedTested = len(finalists)
	st.Funnel.SpeedPassed = len(qualified)
	m.setStatus(st)
	m.logf(s.ID, "STRICT DONE finalists=%d qualified=%d best=%s %.2fMB/s elapsed=%s",
		len(finalists), len(qualified), qualified[0].IP, qualified[0].SpeedMB, time.Since(started).Round(time.Millisecond))
	return nil
}

func countQualified(rows []Result) int {
	n := 0
	for _, r := range rows {
		if r.SpeedTested && r.Qualified {
			n++
		}
	}
	return n
}

func sortObservedForDisplay(rows []Result) []Result {
	out := append([]Result(nil), rows...)
	rank := func(r Result) int {
		switch {
		case r.SpeedTested && r.Qualified:
			return 0
		case !r.SpeedTested:
			return 1
		default:
			return 2
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := rank(out[i]), rank(out[j])
		if ri != rj {
			return ri < rj
		}
		if ri == 0 || ri == 2 {
			if out[i].SpeedMB != out[j].SpeedMB {
				return out[i].SpeedMB > out[j].SpeedMB
			}
		}
		return out[i].LatencyMS < out[j].LatencyMS
	})
	return out
}

func compactReason(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len([]rune(s)) > 120 {
		r := []rune(s)
		return string(r[:120]) + "…"
	}
	return s
}

func (m *Manager) filterByRegion(ctx context.Context, sourceID string, s config.Source, rows []Result) []Result {
	wanted := map[string]bool{}
	for _, c := range s.CFST.Colo {
		c = strings.ToUpper(strings.TrimSpace(c))
		if c != "" {
			wanted[c] = true
		}
	}
	filtering := len(wanted) > 0

	type item struct {
		idx int
		row Result
	}
	jobs := make(chan item)
	var wg sync.WaitGroup
	var mu sync.Mutex
	passed := make([]Result, 0, len(rows))
	regionCounts := map[string]int{}
	unknownCount := 0
	done := 0

	workers := 64
	if len(rows) < workers {
		workers = len(rows)
	}
	if workers < 1 {
		workers = 1
	}

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobs {
				r := job.row
				colo, err := fetchColo(ctx, s, r.IP)
				r.Colo = colo
				regionKey := strings.ToUpper(strings.TrimSpace(colo))
				if filtering {
					if err != nil {
						r.RejectReason = "地区探测失败：" + compactReason(err.Error())
					} else if wanted[regionKey] {
						r.RejectReason = "待速度决赛"
					} else {
						r.RejectReason = fmt.Sprintf("地区 %s 不匹配 %s", colo, strings.Join(s.CFST.Colo, ","))
					}
				} else {
					// Informational-only Colo lookup must never eliminate a candidate.
					r.RejectReason = "待速度决赛"
				}
				m.updateHistoryRow(sourceID, r)

				mu.Lock()
				done++
				if err != nil || regionKey == "" {
					unknownCount++
				} else {
					regionCounts[regionKey]++
				}
				if !filtering || (err == nil && wanted[regionKey]) {
					passed = append(passed, r)
				}
				currentDone := done
				currentPassed := len(passed)
				snapshot := append([]Result(nil), passed...)
				mu.Unlock()

				m.patchProgress(sourceID, ScanProgress{
					Phase:     "region",
					Current:   currentDone,
					Total:     len(rows),
					Available: currentPassed,
				})
				if currentDone%20 == 0 || currentDone == len(rows) {
					m.logf(sourceID, "REGION progress %d/%d matched=%d", currentDone, len(rows), currentPassed)
				}
				if currentDone%10 == 0 || currentDone == len(rows) {
					sort.SliceStable(snapshot, func(i, j int) bool { return snapshot[i].LatencyMS < snapshot[j].LatencyMS })
					m.setObserved(sourceID, snapshot)
				}
			}
		}()
	}
	for i, r := range rows {
		select {
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return nil
		case jobs <- item{idx: i, row: r}:
		}
	}
	close(jobs)
	wg.Wait()

	sort.SliceStable(passed, func(i, j int) bool { return passed[i].LatencyMS < passed[j].LatencyMS })
	funnel := m.currentStatus(sourceID).Funnel
	funnel.RegionPassed = len(passed)
	m.patchFunnel(sourceID, funnel)
	m.setObserved(sourceID, passed)
	keys := make([]string, 0, len(regionCounts))
	for k := range regionCounts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys)+1)
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s:%d", k, regionCounts[k]))
	}
	if unknownCount > 0 {
		parts = append(parts, fmt.Sprintf("UNKNOWN:%d", unknownCount))
	}
	if len(parts) == 0 {
		parts = append(parts, "NONE:0")
	}
	if filtering {
		m.logf(sourceID, "REGION done matched=%d/%d wanted=%s distribution=%s",
			len(passed), len(rows), strings.Join(s.CFST.Colo, ","), strings.Join(parts, ","))
	} else {
		m.logf(sourceID, "REGION info resolved=%d/%d distribution=%s",
			len(rows)-unknownCount, len(rows), strings.Join(parts, ","))
	}
	return passed
}

func effectiveProbeURL(s config.Source) string {
	if v := strings.TrimSpace(s.CFST.ProbeURL); v != "" {
		return normalizeHTTPURL(v, "https")
	}
	if v := strings.TrimSpace(s.GlobalProbeURL); v != "" {
		return normalizeHTTPURL(v, "https")
	}
	return config.DefaultProbeURL
}

func effectiveSpeedURL(s config.Source) string {
	if v := strings.TrimSpace(s.CFST.URL); v != "" {
		return normalizeHTTPURL(v, "https")
	}
	if v := strings.TrimSpace(s.GlobalSpeedURL); v != "" {
		return normalizeHTTPURL(v, "https")
	}
	return config.DefaultSpeedURL
}

func normalizeHTTPURL(v, scheme string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if !strings.HasPrefix(strings.ToLower(v), "http://") && !strings.HasPrefix(strings.ToLower(v), "https://") {
		v = scheme + "://" + v
	}
	return v
}

func fetchColo(ctx context.Context, s config.Source, ip string) (string, error) {
	rawCandidates := []string{}
	if raw := strings.TrimSpace(effectiveProbeURL(s)); raw != "" {
		rawCandidates = append(rawCandidates, raw)
	}
	rawCandidates = append(rawCandidates,
		"https://speed.cloudflare.com/cdn-cgi/trace",
		"https://www.cloudflare.com/cdn-cgi/trace",
	)

	seen := map[string]bool{}
	errs := make([]string, 0, len(rawCandidates))
	for _, raw := range rawCandidates {
		raw = normalizeHTTPURL(raw, "https")
		if raw == "" || seen[raw] {
			continue
		}
		seen[raw] = true
		colo, err := fetchColoAt(ctx, ip, raw, s.CFST.Port)
		if err == nil && colo != "" {
			return colo, nil
		}
		if err != nil {
			errs = append(errs, compactReason(err.Error()))
		}
	}
	if len(errs) == 0 {
		return "", fmt.Errorf("地区探测未取得 Colo")
	}
	if len(errs) > 3 {
		errs = errs[:3]
	}
	return "", fmt.Errorf("地区探测未取得 Colo：%s", strings.Join(errs, " | "))
}

func fetchColoAt(ctx context.Context, ip, raw string, defaultPort int) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return "", fmt.Errorf("invalid probe URL %q", raw)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("probe URL must use http/https")
	}

	port := 0
	if p := u.Port(); p != "" {
		port, _ = strconv.Atoi(p)
	}
	if port <= 0 && defaultPort > 0 {
		port = defaultPort
	}
	if port <= 0 {
		if scheme == "https" {
			port = 443
		} else {
			port = 80
		}
	}

	client := boundHTTPClient(ip, port, u.Hostname(), 3500*time.Millisecond)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "BestIP-Manager/colo")
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return "", fmt.Errorf("%s HTTP %s", u.Hostname(), resp.Status)
	}

	b, err := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToLower(line), "colo=") {
			colo := strings.ToUpper(strings.TrimSpace(line[strings.Index(line, "=")+1:]))
			if len(colo) == 3 {
				return colo, nil
			}
		}
	}
	if ray := strings.TrimSpace(resp.Header.Get("CF-Ray")); ray != "" {
		if i := strings.LastIndex(ray, "-"); i >= 0 && i+1 < len(ray) {
			colo := strings.ToUpper(strings.TrimSpace(ray[i+1:]))
			if len(colo) == 3 {
				return colo, nil
			}
		}
	}
	return "", fmt.Errorf("%s 未返回 colo/CF-Ray", u.Hostname())
}

func measureDownloadSpeed(ctx context.Context, s config.Source, ip string) (float64, error) {
	raw := effectiveSpeedURL(s)
	if raw == "" {
		raw = config.DefaultSpeedURL
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return 0, fmt.Errorf("invalid speed URL %q", raw)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return 0, fmt.Errorf("speed URL must use http/https")
	}
	port := s.CFST.Port
	if port <= 0 {
		if p := u.Port(); p != "" {
			port, _ = strconv.Atoi(p)
		}
	}
	if port <= 0 {
		if scheme == "https" {
			port = 443
		} else {
			port = 80
		}
	}
	seconds := s.CFST.DownloadTime
	if seconds <= 0 {
		seconds = 10
	}

	testCtx, cancel := context.WithTimeout(ctx, time.Duration(seconds+3)*time.Second)
	defer cancel()
	client := boundHTTPClient(ip, port, u.Hostname(), time.Duration(seconds+3)*time.Second)
	req, err := http.NewRequestWithContext(testCtx, http.MethodGet, u.String(), nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", "BestIP-Manager/strict")
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return 0, fmt.Errorf("download HTTP %s", resp.Status)
	}

	start := time.Now()
	deadline := start.Add(time.Duration(seconds) * time.Second)
	buf := make([]byte, 128*1024)
	var total int64
	for {
		if time.Now().After(deadline) {
			break
		}
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			total += int64(n)
		}
		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			// A timeout after receiving data still yields a meaningful average.
			if total > 0 {
				break
			}
			return 0, readErr
		}
	}
	elapsed := time.Since(start).Seconds()
	if elapsed <= 0 || total <= 0 {
		return 0, fmt.Errorf("download returned no data")
	}
	return float64(total) / elapsed / 1024 / 1024, nil
}

func boundHTTPClient(ip string, port int, serverName string, timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: 2 * time.Second, KeepAlive: -1}
	tr := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, net.JoinHostPort(ip, strconv.Itoa(port)))
		},
		DisableKeepAlives: true,
		TLSClientConfig: &tls.Config{
			ServerName: serverName,
			MinVersion: tls.VersionTLS12,
		},
	}
	return &http.Client{Transport: tr, Timeout: timeout}
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

func (m *Manager) patchCandidateCount(id string, n int) {
	m.mu.Lock()
	st := m.status[id]
	st.CandidateCount = n
	st.LastUpdate = time.Now()
	m.status[id] = st
	m.mu.Unlock()
}

func (m *Manager) setHistory(id string, rows []Result) {
	m.mu.Lock()
	m.history[id] = append([]Result(nil), rows...)
	m.mu.Unlock()
}

func (m *Manager) updateHistoryRow(id string, row Result) {
	m.mu.Lock()
	rows := m.history[id]
	for i := range rows {
		if rows[i].IP == row.IP {
			if row.TestedAt.IsZero() {
				row.TestedAt = rows[i].TestedAt
			}
			rows[i] = row
			m.history[id] = rows
			m.mu.Unlock()
			return
		}
	}
	m.history[id] = append(rows, row)
	m.mu.Unlock()
}

func (m *Manager) updateHistoryRowSlice(rows []Result, ip string, fn func(*Result)) {
	for i := range rows {
		if rows[i].IP == ip {
			fn(&rows[i])
			return
		}
	}
}

func (m *Manager) patchProgress(id string, p ScanProgress) {
	now := time.Now()
	m.mu.Lock()
	st := m.status[id]
	prev := st.Progress
	if p.Total > 0 {
		p.Percent = p.Current * 100 / p.Total
		if p.Percent > 100 {
			p.Percent = 100
		}
	}
	if p.StartedAt.IsZero() {
		if prev.Phase == p.Phase && !prev.StartedAt.IsZero() {
			p.StartedAt = prev.StartedAt
		} else {
			p.StartedAt = now
		}
	}
	if !p.StartedAt.IsZero() {
		elapsed := int(now.Sub(p.StartedAt).Seconds())
		if elapsed < 0 { elapsed = 0 }
		p.ElapsedSeconds = elapsed
		if p.Current > 0 && p.Total > p.Current && elapsed > 0 {
			p.ETASeconds = int(float64(elapsed) * float64(p.Total-p.Current) / float64(p.Current))
		}
	}
	st.Progress = p
	st.LastUpdate = now
	m.status[id] = st
	m.mu.Unlock()
}

func (m *Manager) patchFunnel(id string, f FunnelStats) {
	m.mu.Lock()
	st := m.status[id]
	st.Funnel = f
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
	if errors.Is(err, context.Canceled) {
		m.mu.Lock()
		st := m.status[id]
		st.Running = false
		st.Stage = "已停止"
		st.Error = ""
		st.EndedAt = time.Now()
		st.LastUpdate = time.Now()
		m.status[id] = st
		m.mu.Unlock()
		m.logf(id, "STOPPED")
		return err
	}
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
		args = append(args, "-n", strconv.Itoa(c.Threads))
	}
	if c.PingCount > 0 {
		args = append(args, "-t", strconv.Itoa(c.PingCount))
	}
	if c.Port > 0 {
		args = append(args, "-tp", strconv.Itoa(c.Port))
	}
	// v0.6.1: fast latency/loss pre-scan is always TCPing.
	// HTTPing can create false negatives when an HTTP Host/SNI is forced onto
	// arbitrary candidate IPs, so region detection is handled separately.
	// Do not pass -url or -httping here, even for legacy httping=true configs.
	// Deliberately omit -tl/-tll/-tlr/-sl/-cfcolo so the pre-scan preserves
	// responsive IPs that later fail user thresholds.
	return args
}

func buildArgs(s config.Source, inputFile, outFile string) []string {
	c := s.CFST
	args := []string{"-f", inputFile, "-o", outFile}
	if c.Threads > 0 {
		args = append(args, "-n", strconv.Itoa(c.Threads))
	}
	if c.PingCount > 0 {
		args = append(args, "-t", strconv.Itoa(c.PingCount))
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
	if u := effectiveSpeedURL(s); u != "" {
		args = append(args, "-url", u)
	}
	// With a region filter, use TCPing + download and filter the CSV's actual
	// Colo afterwards. This avoids losing candidates during HTTP HEAD pre-filtering.
	if c.HTTPing && len(c.Colo) == 0 {
		args = append(args, "-httping")
	}
	// We consume result.csv ourselves; suppress the verbose terminal result table.
	args = append(args, "-p", "0")
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

// diagnoseIPv6Egress distinguishes "the sampled prefix has no usable IP" from
// "this Docker network namespace has no IPv6 route". It is only used after an
// IPv6 probe returned zero rows, so it adds virtually no overhead to healthy runs.
func diagnoseIPv6Egress(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	// Cloudflare public DNS is used only as a routing sanity check; no payload is
	// sent and it is not used for latency/speed ranking.
	d := &net.Dialer{Timeout: 3 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", "[2606:4700:4700::1111]:443")
	if err == nil {
		_ = conn.Close()
		return ""
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "network is unreachable") || strings.Contains(msg, "no route to host") || strings.Contains(msg, "cannot assign requested address") {
		return "容器没有可用 IPv6 出站路由（Docker bridge 常见）；请在飞牛启用 Docker IPv6，或换用包内的 host 网络 Compose"
	}
	return "IPv6 出站自检也失败：" + compactReason(err.Error())
}

func readCandidateHistory(path, family string) ([]Result, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	now := time.Now()
	rows := []Result{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		ip := strings.TrimSpace(sc.Text())
		if net.ParseIP(ip) == nil {
			continue
		}
		rows = append(rows, Result{IP: ip, Family: family, Loss: 1, TestedAt: now, RejectReason: "未通过连通性初筛"})
	}
	return rows, sc.Err()
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
