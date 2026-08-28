package furnace

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/honghanh691913-glitch/Cloudflare-BestIP/internal/config"
	"github.com/honghanh691913-glitch/Cloudflare-BestIP/internal/engine"
)

type Sample struct {
	Time        time.Time `json:"time"`
	LatencyMS   float64   `json:"latency_ms"`
	Loss        float64   `json:"loss"`
	SpeedMB     float64   `json:"speed_mb"`
	SpeedTested bool      `json:"speed_tested"`
	Colo        string    `json:"colo,omitempty"`
	Hit         bool      `json:"hit"`
	Reason      string    `json:"reason,omitempty"`
}

type Profile struct {
	SourceID       string    `json:"source_id"`
	IP             string    `json:"ip"`
	Family         string    `json:"family"`
	Colo           string    `json:"colo,omitempty"`
	Admitted       bool      `json:"admitted"`
	FirstSeen      time.Time `json:"first_seen"`
	FirstQualified time.Time `json:"first_qualified,omitempty"`
	LastSeen       time.Time `json:"last_seen"`
	Attempts       int       `json:"attempts"`
	Hits           int       `json:"hits"`
	LastLatencyMS  float64   `json:"last_latency_ms"`
	LastLoss       float64   `json:"last_loss"`
	LastSpeedMB    float64   `json:"last_speed_mb"`
	Samples        []Sample  `json:"samples,omitempty"`
}

type persisted struct {
	Version  int                 `json:"version"`
	Updated  time.Time           `json:"updated_at"`
	Profiles map[string]*Profile `json:"profiles"`
}

type Store struct {
	path string
	mu   sync.RWMutex
	data persisted
}

type Summary struct {
	SourceID     string    `json:"source_id"`
	IP           string    `json:"ip"`
	Family       string    `json:"family"`
	Colo         string    `json:"colo,omitempty"`
	Admitted     bool      `json:"admitted"`
	Phenotype    string    `json:"phenotype"`
	Attempts     int       `json:"attempts"`
	Hits         int       `json:"hits"`
	HitRate      float64   `json:"hit_rate"`
	Maturity     int       `json:"maturity"`
	AvgLatencyMS float64   `json:"avg_latency_ms"`
	AvgLoss      float64   `json:"avg_loss"`
	AvgSpeedMB   float64   `json:"avg_speed_mb"`
	DayScore     float64   `json:"day_score"`
	NightScore   float64   `json:"night_score"`
	CurrentScore float64   `json:"current_score"`
	BestHour     int       `json:"best_hour"`
	WorstHour    int       `json:"worst_hour"`
	LastSeen     time.Time `json:"last_seen"`
	GrayLevel    int       `json:"gray_level"`
}

type Detail struct {
	Summary Summary  `json:"summary"`
	Samples []Sample `json:"samples"`
}

func New(path string) (*Store, error) {
	s := &Store{path: path, data: persisted{Version: 1, Profiles: map[string]*Profile{}}}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	if len(b) == 0 {
		return s, nil
	}
	if err := json.Unmarshal(b, &s.data); err != nil {
		return nil, fmt.Errorf("load furnace: %w", err)
	}
	if s.data.Profiles == nil {
		s.data.Profiles = map[string]*Profile{}
	}
	return s, nil
}

func DefaultPath(configPath string) string {
	dir := filepath.Dir(configPath)
	if strings.TrimSpace(dir) == "" || dir == "." {
		dir = "/data"
	}
	return filepath.Join(dir, "furnace.json")
}

func key(sourceID, ip string) string { return sourceID + "|" + ip }

func qualifies(r engine.Result, rule config.FurnaceRule) bool {
	if !r.SpeedTested {
		return false
	}
	if rule.LatencyMaxMS > 0 && r.LatencyMS > rule.LatencyMaxMS {
		return false
	}
	if rule.LossMax >= 0 && r.Loss > rule.LossMax {
		return false
	}
	if rule.SpeedMinMB > 0 && r.SpeedMB < rule.SpeedMinMB {
		return false
	}
	return true
}

func (s *Store) Ingest(sourceID, family string, rows []engine.Result, rule config.FurnaceRule, retentionDays int) error {
	if !rule.Enabled || strings.TrimSpace(sourceID) == "" || len(rows) == 0 {
		return nil
	}
	if retentionDays <= 0 {
		retentionDays = 45
	}
	now := time.Now()
	cutoff := now.Add(-time.Duration(retentionDays) * 24 * time.Hour)

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range rows {
		if strings.TrimSpace(r.IP) == "" {
			continue
		}
		k := key(sourceID, r.IP)
		p := s.data.Profiles[k]
		if p == nil {
			p = &Profile{SourceID: sourceID, IP: r.IP, Family: family, FirstSeen: now}
			s.data.Profiles[k] = p
		}
		t := r.TestedAt
		if t.IsZero() {
			t = now
		}
		hit := qualifies(r, rule)
		p.Attempts++
		if hit {
			p.Hits++
			if !p.Admitted {
				p.Admitted = true
				p.FirstQualified = t
			}
		}
		p.LastSeen = t
		p.LastLatencyMS = r.LatencyMS
		p.LastLoss = r.Loss
		if r.SpeedTested {
			p.LastSpeedMB = r.SpeedMB
		}
		if strings.TrimSpace(r.Colo) != "" {
			p.Colo = strings.ToUpper(strings.TrimSpace(r.Colo))
		}
		sample := Sample{Time: t, LatencyMS: r.LatencyMS, Loss: r.Loss, SpeedMB: r.SpeedMB, SpeedTested: r.SpeedTested, Colo: r.Colo, Hit: hit, Reason: r.RejectReason}
		// Keep a tiny pre-admission tail so a newly admitted IP has context. Once
		// admitted, retain enough samples for roughly 1-2 months of 6-hour scans.
		p.Samples = append(p.Samples, sample)
		kept := p.Samples[:0]
		for _, sm := range p.Samples {
			if sm.Time.After(cutoff) || sm.Time.Equal(cutoff) {
				kept = append(kept, sm)
			}
		}
		p.Samples = kept
		limit := 256
		if !p.Admitted {
			limit = 8
		}
		if len(p.Samples) > limit {
			p.Samples = append([]Sample(nil), p.Samples[len(p.Samples)-limit:]...)
		}
	}
	s.data.Updated = now
	return s.saveLocked()
}

func (s *Store) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *Store) Summaries(c config.Config, now time.Time) []Summary {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Summary, 0, len(s.data.Profiles))
	for _, p := range s.data.Profiles {
		rule, ok := config.FurnaceRuleFor(c, p.SourceID)
		if !ok || !rule.Enabled {
			continue
		}
		out = append(out, summarize(*p, rule, now))
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Admitted != out[j].Admitted {
			return out[i].Admitted
		}
		if out[i].CurrentScore != out[j].CurrentScore {
			return out[i].CurrentScore > out[j].CurrentScore
		}
		return out[i].HitRate > out[j].HitRate
	})
	return out
}

func (s *Store) Detail(c config.Config, sourceID, ip string, now time.Time) (Detail, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p := s.data.Profiles[key(sourceID, ip)]
	if p == nil {
		return Detail{}, false
	}
	rule, ok := config.FurnaceRuleFor(c, sourceID)
	if !ok {
		rule = config.FurnaceRule{SourceID: sourceID, Enabled: true, LatencyMaxMS: 50, LossMax: 0, SpeedMinMB: 50}
	}
	return Detail{Summary: summarize(*p, rule, now), Samples: append([]Sample(nil), p.Samples...)}, true
}

func summarize(p Profile, rule config.FurnaceRule, now time.Time) Summary {
	var latSum, lossSum, speedSum float64
	var n, speedN int
	hourScore := [24]float64{}
	hourCount := [24]int{}
	var daySum, nightSum float64
	var dayN, nightN int
	qualityVals := []float64{}
	for _, sm := range p.Samples {
		if sm.LatencyMS > 0 || sm.Loss > 0 || sm.SpeedTested {
			latSum += sm.LatencyMS
			lossSum += sm.Loss
			n++
		}
		if sm.SpeedTested {
			speedSum += sm.SpeedMB
			speedN++
		}
		q := sampleScore(sm, rule)
		if sm.SpeedTested || sm.LatencyMS > 0 || sm.Loss >= 1 {
			h := sm.Time.Local().Hour()
			hourScore[h] += q
			hourCount[h]++
			qualityVals = append(qualityVals, q)
			if h >= 6 && h < 18 {
				daySum += q
				dayN++
			} else {
				nightSum += q
				nightN++
			}
		}
	}
	avgLat, avgLoss, avgSpeed := 0.0, 0.0, 0.0
	if n > 0 {
		avgLat = latSum / float64(n)
		avgLoss = lossSum / float64(n)
	}
	if speedN > 0 {
		avgSpeed = speedSum / float64(speedN)
	}
	dayScore, nightScore := avg(daySum, dayN), avg(nightSum, nightN)
	bestHour, worstHour := -1, -1
	best, worst := -1.0, 101.0
	for h := 0; h < 24; h++ {
		if hourCount[h] == 0 {
			continue
		}
		v := hourScore[h] / float64(hourCount[h])
		if v > best {
			best, bestHour = v, h
		}
		if v < worst {
			worst, worstHour = v, h
		}
	}
	periodScore := dayScore
	if now.Local().Hour() < 6 || now.Local().Hour() >= 18 {
		periodScore = nightScore
	}
	if periodScore == 0 && len(qualityVals) > 0 {
		for _, v := range qualityVals {
			periodScore += v
		}
		periodScore /= float64(len(qualityVals))
	}
	hitRate := 0.0
	if p.Attempts > 0 {
		hitRate = float64(p.Hits) / float64(p.Attempts)
	}
	maturity := p.Attempts * 100 / 28
	if maturity > 100 {
		maturity = 100
	}
	gray := 100 - int(math.Round(hitRate*100))
	if p.Admitted {
		gray /= 2
	}
	return Summary{
		SourceID: p.SourceID, IP: p.IP, Family: p.Family, Colo: p.Colo, Admitted: p.Admitted,
		Phenotype: phenotype(dayScore, nightScore, qualityVals, p.Attempts, hitRate), Attempts: p.Attempts, Hits: p.Hits,
		HitRate: hitRate, Maturity: maturity, AvgLatencyMS: avgLat, AvgLoss: avgLoss, AvgSpeedMB: avgSpeed,
		DayScore: dayScore, NightScore: nightScore, CurrentScore: periodScore, BestHour: bestHour, WorstHour: worstHour,
		LastSeen: p.LastSeen, GrayLevel: gray,
	}
}

func avg(sum float64, n int) float64 {
	if n == 0 {
		return 0
	}
	return sum / float64(n)
}

func sampleScore(sm Sample, rule config.FurnaceRule) float64 {
	latTarget := rule.LatencyMaxMS
	if latTarget <= 0 {
		latTarget = 100
	}
	speedTarget := rule.SpeedMinMB
	if speedTarget <= 0 {
		speedTarget = 10
	}
	latScore := 1.0
	if sm.LatencyMS > 0 {
		latScore = math.Min(1.2, latTarget/sm.LatencyMS)
	}
	speedScore := 0.0
	if sm.SpeedTested {
		speedScore = math.Min(1.5, sm.SpeedMB/speedTarget)
	}
	lossScore := 1.0 - math.Min(1, sm.Loss*5)
	if sm.Loss >= 1 {
		lossScore = 0
	}
	score := (latScore*35 + speedScore*45 + lossScore*20)
	if !sm.SpeedTested {
		score *= 0.55
	}
	if score < 0 {
		return 0
	}
	if score > 100 {
		return 100
	}
	return score
}

func phenotype(day, night float64, vals []float64, attempts int, hitRate float64) string {
	if attempts < 8 || len(vals) < 4 {
		return "学习中"
	}
	mean := 0.0
	for _, v := range vals {
		mean += v
	}
	mean /= float64(len(vals))
	variance := 0.0
	for _, v := range vals {
		d := v - mean
		variance += d * d
	}
	variance /= float64(len(vals))
	cv := 0.0
	if mean > 0 {
		cv = math.Sqrt(variance) / mean
	}
	if cv > 0.38 {
		return "波动型"
	}
	if day > 0 && night > 0 {
		if day > night*1.12 {
			return "日间型"
		}
		if night > day*1.12 {
			return "夜间型"
		}
	}
	if hitRate >= 0.8 && cv < 0.24 {
		return "全天稳定"
	}
	return "均衡型"
}

// Rank reorders only currently-qualified fresh results. Historical learning can
// choose among safe candidates, but it can never resurrect a stale/failed IP.
func (s *Store) Rank(c config.Config, sourceID string, rows []engine.Result, now time.Time) []engine.Result {
	out := append([]engine.Result(nil), rows...)
	if !c.FurnaceAutoRank || len(out) < 2 {
		return out
	}
	rule, ok := config.FurnaceRuleFor(c, sourceID)
	if !ok || !rule.Enabled || !rule.AutoRank {
		return out
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	type ranked struct {
		r engine.Result
		s float64
	}
	ranks := make([]ranked, 0, len(out))
	for _, r := range out {
		base := 50.0
		if rule.SpeedMinMB > 0 {
			base = math.Min(100, r.SpeedMB/rule.SpeedMinMB*60)
		}
		p := s.data.Profiles[key(sourceID, r.IP)]
		historical := 0.0
		maturity := 0
		if p != nil {
			sm := summarize(*p, rule, now)
			historical = sm.CurrentScore
			maturity = sm.Maturity
		}
		weight := math.Min(0.45, float64(maturity)/100*0.45)
		score := base*(1-weight) + historical*weight
		ranks = append(ranks, ranked{r: r, s: score})
	}
	sort.SliceStable(ranks, func(i, j int) bool {
		if ranks[i].s == ranks[j].s {
			return ranks[i].r.SpeedMB > ranks[j].r.SpeedMB
		}
		return ranks[i].s > ranks[j].s
	})
	for i := range ranks {
		out[i] = ranks[i].r
	}
	return out
}

func PeriodName(t time.Time) string {
	h := t.Local().Hour()
	if h >= 6 && h < 18 {
		return "day"
	}
	return "night"
}
