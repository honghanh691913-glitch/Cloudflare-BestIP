package engine

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/honghanh691913-glitch/Cloudflare-BestIP/internal/config"
	"github.com/honghanh691913-glitch/Cloudflare-BestIP/internal/reallink"
)

type HealthReport struct {
	Checked  int      `json:"checked"`
	Healthy  int      `json:"healthy"`
	Rows     []Result `json:"rows"`
	Canceled bool     `json:"canceled,omitempty"`
}

// CheckHealth revalidates current DNS candidates using the same three hard
// constraints that decide whether an IP may stay in service: latency/loss,
// requested Colo, and minimum download speed. It intentionally checks only the
// small active set supplied by the caller rather than rescanning the whole CIDR.
func (m *Manager) CheckHealth(ctx context.Context, s config.Source, current []Result) HealthReport {
	var ok bool
	ctx, ok = m.beginRun(ctx, s.ID)
	if !ok {
		return HealthReport{}
	}
	defer m.endRun(s.ID)

	prev := m.currentStatus(s.ID)
	started := time.Now()
	m.setStatus(SourceStatus{
		SourceID: s.ID, Running: true, Stage: "健康检查", StartedAt: started, LastUpdate: started,
		Results: append([]Result(nil), prev.Results...), Observed: []Result{}, Logs: append([]string(nil), prev.Logs...),
	})
	m.patchProgress(s.ID, ScanProgress{Phase: "health", Current: 0, Total: len(current)})
	m.logf(s.ID, "HEALTH START active=%d thresholds(latency<=%.0fms loss<=%.2f speed>=%.2fMB/s colo=%s)",
		len(current), s.CFST.LatencyMaxMS, s.CFST.LossMax, s.CFST.SpeedMinMB, strings.Join(s.CFST.Colo, ","))

	report := HealthReport{Rows: make([]Result, 0, len(current))}
	for i, old := range current {
		if err := ctx.Err(); err != nil {
			break
		}
		r := checkHealthOnce(ctx, s, old)
		// A current production IP is not evicted on one transient sample.
		// Only a second consecutive failure in the same health cycle confirms replacement.
		if !r.Qualified && ctx.Err() == nil {
			m.logf(s.ID, "HEALTH RETRY ip=%s first_failure=%s", r.IP, r.RejectReason)
			select {
			case <-ctx.Done():
			case <-time.After(1500 * time.Millisecond):
				r2 := checkHealthOnce(ctx, s, old)
				if r2.Qualified {
					m.logf(s.ID, "HEALTH RECOVERED ip=%s second_check_passed", r2.IP)
					r = r2
				} else if ctx.Err() == nil {
					r = r2
					r.RejectReason = "连续两次不达标：" + r2.RejectReason
				}
			}
		}
		report.Checked++
		if r.Qualified {
			report.Healthy++
		}
		report.Rows = append(report.Rows, r)
		m.setObserved(s.ID, append([]Result(nil), report.Rows...))
		m.patchProgress(s.ID, ScanProgress{Phase: "health", Current: i + 1, Total: len(current), Available: report.Healthy})
		m.logf(s.ID, "HEALTH %d/%d ip=%s healthy=%v colo=%s latency=%.1fms loss=%.0f%% speed=%.2fMB/s reason=%s",
			i+1, len(current), r.IP, r.Qualified, r.Colo, r.LatencyMS, r.Loss*100, r.SpeedMB, r.RejectReason)
	}

	if ctx.Err() != nil {
		report.Canceled = true
		st := m.currentStatus(s.ID)
		st.Running = false
		st.Stage = "已停止"
		st.Error = ""
		st.EndedAt = time.Now()
		st.LastUpdate = time.Now()
		st.Observed = append([]Result(nil), report.Rows...)
		st.ObservedTotal = len(report.Rows)
		m.setStatus(st)
		m.logf(s.ID, "HEALTH STOPPED checked=%d/%d", report.Checked, len(current))
		return report
	}

	st := m.currentStatus(s.ID)
	st.Running = false
	st.Stage = "健康检查完成"
	st.EndedAt = time.Now()
	st.LastUpdate = time.Now()
	st.Observed = append([]Result(nil), report.Rows...)
	st.ObservedTotal = len(report.Rows)
	st.Progress = ScanProgress{Phase: "health", Current: report.Checked, Total: len(current), Available: report.Healthy, Percent: func() int {
		if len(current) > 0 {
			return report.Checked * 100 / len(current)
		}
		return 100
	}(), StartedAt: started, ElapsedSeconds: int(time.Since(started).Seconds())}
	m.setStatus(st)
	m.logf(s.ID, "HEALTH DONE checked=%d healthy=%d/%d elapsed=%s", report.Checked, report.Healthy, len(current), time.Since(started).Round(time.Millisecond))
	return report
}

func checkHealthOnce(ctx context.Context, s config.Source, old Result) Result {
	r := old
	r.TestedAt = time.Now()
	r.Qualified = false
	r.SpeedTested = false
	r.RejectReason = ""

	latency, loss := measureLatencyLoss(ctx, r.IP, s.CFST.Port, maxInt(s.CFST.PingCount, 4))
	r.LatencyMS = latency
	r.Loss = loss
	if ctx.Err() != nil {
		r.RejectReason = "健康检查：已停止"
		return r
	}
	if loss >= 1 {
		r.RejectReason = "健康检查：无法连接"
	} else if s.CFST.LatencyMaxMS > 0 && latency > s.CFST.LatencyMaxMS {
		r.RejectReason = fmt.Sprintf("健康检查：延迟 %.1fms > %.1fms", latency, s.CFST.LatencyMaxMS)
	} else if s.CFST.LossMax >= 0 && loss > s.CFST.LossMax {
		r.RejectReason = fmt.Sprintf("健康检查：丢包 %.0f%% > %.0f%%", loss*100, s.CFST.LossMax*100)
	} else if len(s.CFST.Colo) > 0 {
		colo, err := fetchColo(ctx, s, r.IP)
		if err != nil {
			r.RejectReason = "健康检查：地区探测失败"
		} else {
			r.Colo = colo
			if !containsColo(s.CFST.Colo, colo) {
				r.RejectReason = fmt.Sprintf("健康检查：地区 %s 不匹配 %s", colo, strings.Join(s.CFST.Colo, ","))
			}
		}
	} else if strings.TrimSpace(r.Colo) == "" {
		if colo, err := fetchColo(ctx, s, r.IP); err == nil {
			r.Colo = colo
		}
	}
	if r.RejectReason == "" && s.RealProfile != nil {
		latency, err := reallink.MeasureLatency(ctx, *s.RealProfile, r.IP, s.GlobalRealTestURL, s.GlobalRealAttempts)
		r.RealTested = true
		if err != nil {
			r.RejectReason = "健康检查：真连接失败"
		} else {
			r.RealLatencyMS = latency
			if s.RealLatencyMaxMS > 0 && latency > s.RealLatencyMaxMS {
				r.RejectReason = fmt.Sprintf("健康检查：真延迟 %.1fms > %.1fms", latency, s.RealLatencyMaxMS)
			}
		}
	}
	if r.RejectReason == "" {
		speed, err := measureDownloadSpeed(ctx, s, r.IP)
		r.SpeedTested = true
		if err != nil {
			r.RejectReason = "健康检查：测速失败"
		} else {
			r.SpeedMB = speed
			if s.CFST.SpeedMinMB > 0 && speed < s.CFST.SpeedMinMB {
				r.RejectReason = fmt.Sprintf("健康检查：速度 %.2fMB/s < %.2fMB/s", speed, s.CFST.SpeedMinMB)
			}
		}
	}
	if r.RejectReason == "" {
		r.Qualified = true
	}
	return r
}

func measureLatencyLoss(ctx context.Context, ip string, port, attempts int) (float64, float64) {
	if port <= 0 {
		port = 443
	}
	if attempts <= 0 {
		attempts = 4
	}
	if attempts > 10 {
		attempts = 10
	}
	var success int
	var total time.Duration
	for i := 0; i < attempts; i++ {
		if ctx.Err() != nil {
			break
		}
		start := time.Now()
		d := &net.Dialer{Timeout: time.Second}
		conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(ip, strconv.Itoa(port)))
		if err != nil {
			continue
		}
		total += time.Since(start)
		success++
		_ = conn.Close()
	}
	if success == 0 {
		return 0, 1
	}
	return float64(total.Microseconds()) / 1000 / float64(success), float64(attempts-success) / float64(attempts)
}

func containsColo(wanted []string, got string) bool {
	got = strings.ToUpper(strings.TrimSpace(got))
	for _, v := range wanted {
		if strings.ToUpper(strings.TrimSpace(v)) == got {
			return true
		}
	}
	return false
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// ApplyHealthyRefresh updates the stored latest result metrics only when every
// checked row is still healthy. That keeps DNS from ever shrinking to a partial
// set while still letting healthy periodic checks refresh ordering by speed.
func (m *Manager) ApplyHealthyRefresh(sourceID string, rows []Result) bool {
	if len(rows) == 0 {
		return false
	}
	for _, r := range rows {
		if !r.Qualified {
			return false
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.status[sourceID]
	if len(st.Results) == 0 {
		return false
	}
	byIP := map[string]Result{}
	for _, r := range rows {
		byIP[r.IP] = r
	}
	for i := range st.Results {
		if fresh, ok := byIP[st.Results[i].IP]; ok {
			st.Results[i] = fresh
		}
	}
	sort.SliceStable(st.Results, func(i, j int) bool {
		if st.Results[i].SpeedMB == st.Results[j].SpeedMB {
			return st.Results[i].LatencyMS < st.Results[j].LatencyMS
		}
		return st.Results[i].SpeedMB > st.Results[j].SpeedMB
	})
	st.LastUpdate = time.Now()
	m.status[sourceID] = st
	return true
}

// SeedResults hydrates the in-memory source state from records that already
// exist in DNS and have just passed a fresh health check.
func (m *Manager) SeedResults(sourceID string, rows []Result, stage string) {
	if len(rows) == 0 {
		return
	}
	out := append([]Result(nil), rows...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].SpeedMB == out[j].SpeedMB {
			return out[i].LatencyMS < out[j].LatencyMS
		}
		return out[i].SpeedMB > out[j].SpeedMB
	})
	now := time.Now()
	m.mu.Lock()
	st := m.status[sourceID]
	st.SourceID = sourceID
	st.Running = false
	if strings.TrimSpace(stage) == "" {
		stage = "启动恢复完成"
	}
	st.Stage = stage
	st.Error = ""
	st.Results = out
	st.Observed = append([]Result(nil), out...)
	st.ObservedTotal = len(out)
	st.LastUpdate = now
	st.EndedAt = now
	st.Progress = ScanProgress{Phase: "health", Current: len(out), Total: len(out), Available: len(out), Percent: 100}
	m.status[sourceID] = st
	m.mu.Unlock()
}

// MergeResults keeps already-healthy active IPs and fills only missing slots
// from a supplemental strict scan. Duplicates are removed and the final active
// set is ranked by current speed, then latency.
func (m *Manager) MergeResults(sourceID string, healthy, supplemental []Result, required int) []Result {
	if required < 1 {
		required = len(healthy)
	}
	seen := map[string]bool{}
	merged := make([]Result, 0, required)

	// Health refill is a stability operation, not a new election:
	// every currently-healthy DNS IP must survive. Only empty slots are filled.
	for _, r := range healthy {
		if !r.Qualified || strings.TrimSpace(r.IP) == "" || seen[r.IP] {
			continue
		}
		seen[r.IP] = true
		merged = append(merged, r)
		if len(merged) >= required {
			break
		}
	}

	candidates := make([]Result, 0, len(supplemental))
	for _, r := range supplemental {
		if !r.Qualified || strings.TrimSpace(r.IP) == "" || seen[r.IP] {
			continue
		}
		candidates = append(candidates, r)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].SpeedMB == candidates[j].SpeedMB {
			return candidates[i].LatencyMS < candidates[j].LatencyMS
		}
		return candidates[i].SpeedMB > candidates[j].SpeedMB
	})
	for _, r := range candidates {
		if len(merged) >= required {
			break
		}
		seen[r.IP] = true
		merged = append(merged, r)
	}

	// Order may change for display/DNS, but membership of healthy survivors does not.
	sort.SliceStable(merged, func(i, j int) bool {
		if merged[i].SpeedMB == merged[j].SpeedMB {
			return merged[i].LatencyMS < merged[j].LatencyMS
		}
		return merged[i].SpeedMB > merged[j].SpeedMB
	})
	if len(merged) > 0 {
		m.SeedResults(sourceID, merged, "健康补位完成")
	}
	return merged
}
