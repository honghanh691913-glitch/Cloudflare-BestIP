package engine

import (
	"strings"
	"testing"

	"github.com/honghanh691913-glitch/Cloudflare-BestIP/internal/config"
)

func TestBuildArgsColoUsesPostFilter(t *testing.T) {
	s := config.Source{
		Family:      "ipv4",
		KeepResults: 50,
		CFST: config.CFST{
			Threads:       4,
			PingCount:     200,
			DownloadCount: 10,
			LatencyMaxMS:  200,
			LossMax:       0.2,
			SpeedMinMB:    5,
			HTTPing:       true,
			Colo:          []string{"NRT"},
			AllIP:         true,
		},
	}
	args := buildArgs(s, "/tmp/in.txt", "/tmp/out.csv")
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "-cfcolo") {
		t.Fatalf("region must not be passed as CFST pre-filter: %s", joined)
	}
	if strings.Contains(joined, "-httping") {
		t.Fatalf("region task should use TCPing/download post-filter: %s", joined)
	}
	if !strings.Contains(joined, "-allip") {
		t.Fatalf("allip flag missing: %s", joined)
	}
	if !strings.Contains(joined, "-dn 50") {
		t.Fatalf("expected wider result pool: %s", joined)
	}
	if !strings.Contains(joined, "-tl 200") || strings.Contains(joined, "-tl 200.00") {
		t.Fatalf("latency flag format wrong: %s", joined)
	}
}

func TestFilterResultsByColo(t *testing.T) {
	in := []Result{
		{IP: "1.1.1.1", Colo: "NRT"},
		{IP: "2.2.2.2", Colo: "HKG"},
		{IP: "3.3.3.3", Colo: "nrt"},
	}
	out := filterResultsByColo(in, []string{"NRT"})
	if len(out) != 2 {
		t.Fatalf("expected 2 NRT results, got %d", len(out))
	}
}

func TestBuildProbeArgsKeepsRejectedCandidatesVisible(t *testing.T) {
	s := config.Source{
		Family: "ipv4",
		CFST: config.CFST{
			Threads: 4, PingCount: 200, Port: 443,
			LatencyMaxMS: 100, LossMax: 0.1, SpeedMinMB: 40,
			HTTPing: true, Colo: []string{"NRT"}, AllIP: true,
		},
	}
	joined := strings.Join(buildProbeArgs(s, "/tmp/in.txt", "/tmp/probe.csv"), " ")
	for _, forbidden := range []string{"-tl ", "-tll ", "-tlr ", "-sl ", "-cfcolo", "-httping"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("probe must stay relaxed; found %q in %s", forbidden, joined)
		}
	}
	if !strings.Contains(joined, "-dd") || !strings.Contains(joined, "-allip") {
		t.Fatalf("probe flags missing: %s", joined)
	}
}

func TestCFSTStreamUpdatesProgress(t *testing.T) {
	m := NewManager()
	m.setStatus(SourceStatus{SourceID: "s1", Running: true, Results: []Result{}})
	w := newCFSTStream(m, "s1", "probe")
	_, _ = w.Write([]byte("12 / 100 [----] 可用: 9\r"))
	st := m.Snapshot()["s1"]
	if st.Progress.Current != 12 || st.Progress.Total != 100 || st.Progress.Available != 9 || st.Progress.Percent != 12 {
		t.Fatalf("unexpected progress: %+v", st.Progress)
	}
}
