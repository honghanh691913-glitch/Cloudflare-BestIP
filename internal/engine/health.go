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
)

type HealthReport struct {
	Checked int      `json:"checked"`
	Healthy int      `json:"healthy"`
	Rows    []Result `json:"rows"`
}

// CheckHealth revalidates current DNS candidates using the same three hard
// constraints that decide whether an IP may stay in service: latency/loss,
// requested Colo, and minimum download speed. It intentionally checks only the
// small active set supplied by the caller rather than rescanning the whole CIDR.
func (m *Manager) CheckHealth(ctx context.Context, s config.Source, current []Result) HealthReport {
	report := HealthReport{Rows: make([]Result, 0, len(current))}
	for i, old := range current {
		if err := ctx.Err(); err != nil {
			break
		}
		r := old
		r.TestedAt = time.Now()
		r.Qualified = false
		r.SpeedTested = false
		r.RejectReason = ""

		latency, loss := measureLatencyLoss(ctx, r.IP, s.CFST.Port, maxInt(s.CFST.PingCount, 4))
		r.LatencyMS = latency
		r.Loss = loss
		report.Checked++
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
			report.Healthy++
		}
		report.Rows = append(report.Rows, r)
		m.logf(s.ID, "HEALTH %d/%d ip=%s healthy=%v latency=%.1fms loss=%.0f%% speed=%.2fMB/s reason=%s",
			i+1, len(current), r.IP, r.Qualified, r.LatencyMS, r.Loss*100, r.SpeedMB, r.RejectReason)
	}
	return report
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
