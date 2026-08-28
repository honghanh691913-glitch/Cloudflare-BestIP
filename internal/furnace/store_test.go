package furnace

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/honghanh691913-glitch/Cloudflare-BestIP/internal/config"
	"github.com/honghanh691913-glitch/Cloudflare-BestIP/internal/engine"
)

func TestAdmissionAndHitRate(t *testing.T) {
	s, err := New(filepath.Join(t.TempDir(), "furnace.json"))
	if err != nil {
		t.Fatal(err)
	}
	rule := config.FurnaceRule{SourceID: "s1", Enabled: true, LatencyMaxMS: 50, LossMax: 0, SpeedMinMB: 50, AutoRank: true}
	now := time.Now()
	rows := []engine.Result{
		{IP: "1.1.1.1", Family: "ipv4", LatencyMS: 40, Loss: 0, SpeedMB: 60, SpeedTested: true, TestedAt: now},
		{IP: "2.2.2.2", Family: "ipv4", LatencyMS: 80, Loss: 0, SpeedMB: 80, SpeedTested: true, TestedAt: now},
	}
	if err := s.Ingest("s1", "ipv4", rows, rule, 45); err != nil {
		t.Fatal(err)
	}
	c := config.Config{FurnaceRules: []config.FurnaceRule{rule}, FurnaceAutoRank: true}
	sums := s.Summaries(c, now)
	if len(sums) != 2 {
		t.Fatalf("expected 2 profiles, got %d", len(sums))
	}
	var admitted, gray bool
	for _, sm := range sums {
		if sm.IP == "1.1.1.1" && sm.Admitted && sm.HitRate == 1 {
			admitted = true
		}
		if sm.IP == "2.2.2.2" && !sm.Admitted {
			gray = true
		}
	}
	if !admitted || !gray {
		t.Fatalf("unexpected summaries: %+v", sums)
	}
}

func TestRankUsesMatureHistoryOnlyAsWeight(t *testing.T) {
	s, err := New(filepath.Join(t.TempDir(), "furnace.json"))
	if err != nil {
		t.Fatal(err)
	}
	rule := config.FurnaceRule{SourceID: "s1", Enabled: true, LatencyMaxMS: 100, LossMax: 0.1, SpeedMinMB: 10, AutoRank: true}
	base := time.Date(2026, 8, 20, 8, 0, 0, 0, time.Local)
	for i := 0; i < 28; i++ {
		rows := []engine.Result{
			{IP: "1.1.1.1", Family: "ipv4", LatencyMS: 20, Loss: 0, SpeedMB: 80, SpeedTested: true, TestedAt: base.Add(time.Duration(i) * 24 * time.Hour)},
			{IP: "2.2.2.2", Family: "ipv4", LatencyMS: 40, Loss: 0, SpeedMB: 20, SpeedTested: true, TestedAt: base.Add(time.Duration(i) * 24 * time.Hour)},
		}
		if err := s.Ingest("s1", "ipv4", rows, rule, 45); err != nil {
			t.Fatal(err)
		}
	}
	c := config.Config{FurnaceRules: []config.FurnaceRule{rule}, FurnaceAutoRank: true}
	fresh := []engine.Result{{IP: "2.2.2.2", SpeedMB: 90, Qualified: true}, {IP: "1.1.1.1", SpeedMB: 70, Qualified: true}}
	ranked := s.Rank(c, "s1", fresh, base)
	if len(ranked) != 2 {
		t.Fatal("rank length")
	}
	// Current performance still dominates enough that a large fresh advantage is not ignored.
	if ranked[0].IP != "2.2.2.2" {
		t.Fatalf("fresh strict result should remain important: %+v", ranked)
	}
}
