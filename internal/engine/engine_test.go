package engine

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/honghanh691913-glitch/Cloudflare-BestIP/internal/config"
)

func TestBuildProbeArgsIsLightweightAndExactCandidates(t *testing.T) {
	s := config.Source{
		Family: "ipv4",
		CFST: config.CFST{
			Threads: 200, PingCount: 4, Port: 443,
			LatencyMaxMS: 100, LossMax: 0.1, SpeedMinMB: 40,
			HTTPing: true, Colo: []string{"NRT"}, AllIP: true,
		},
	}
	joined := strings.Join(buildProbeArgs(s, "/tmp/in.txt", "/tmp/probe.csv"), " ")
	for _, forbidden := range []string{"-tl ", "-tll ", "-tlr ", "-sl ", "-cfcolo", "-httping"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("light probe must not pre-filter; found %q in %s", forbidden, joined)
		}
	}
	if !strings.Contains(joined, "-dd") {
		t.Fatalf("probe flags missing: %s", joined)
	}
	if strings.Contains(joined, "-allip") {
		t.Fatalf("v0.6 must pass exact sampled IPs, not CFST -allip: %s", joined)
	}
}


func TestBuildProbeArgsForcesTCPEvenWhenLegacyHTTPingEnabled(t *testing.T) {
	s := config.Source{
		Family: "ipv4",
		CFST: config.CFST{
			Threads: 200,
			PingCount: 4,
			Port: 443,
			HTTPing: true,
			ProbeURL: "https://speed.cloudflare.com/cdn-cgi/trace",
		},
	}
	joined := strings.Join(buildProbeArgs(s, "/tmp/in.txt", "/tmp/probe.csv"), " ")
	if strings.Contains(joined, "-httping") || strings.Contains(joined, "-url ") {
		t.Fatalf("pre-scan must be TCP only even for legacy httping=true: %s", joined)
	}
}

func TestSortObservedPinsQualifiedBySpeed(t *testing.T) {
	rows := []Result{
		{IP: "pending", LatencyMS: 20},
		{IP: "slow-good", SpeedTested: true, Qualified: true, SpeedMB: 10, LatencyMS: 30},
		{IP: "bad", SpeedTested: true, Qualified: false, SpeedMB: 2, LatencyMS: 10},
		{IP: "fast-good", SpeedTested: true, Qualified: true, SpeedMB: 50, LatencyMS: 40},
	}
	got := sortObservedForDisplay(rows)
	want := []string{"fast-good", "slow-good", "pending", "bad"}
	for i := range want {
		if got[i].IP != want[i] {
			t.Fatalf("row %d = %s, want %s", i, got[i].IP, want[i])
		}
	}
}

func TestFetchColoAndMeasureSpeedAgainstBoundIP(t *testing.T) {
	payload := strings.Repeat("x", 512*1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/cdn-cgi/trace" {
			fmt.Fprint(w, "fl=1\ncolo=NRT\nip=127.0.0.1\n")
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		_, _ = w.Write([]byte(payload))
	}))
	defer srv.Close()

	u, _ := url.Parse(srv.URL)
	_, portStr, _ := net.SplitHostPort(u.Host)
	port, _ := strconv.Atoi(portStr)

	s := config.Source{CFST: config.CFST{
		URL:          "http://example.test/download",
		ProbeURL:     "http://example.test/cdn-cgi/trace",
		Port:         port,
		DownloadTime: 1,
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	colo, err := fetchColo(ctx, s, "127.0.0.1")
	if err != nil || colo != "NRT" {
		t.Fatalf("colo=%q err=%v", colo, err)
	}
	speed, err := measureDownloadSpeed(ctx, s, "127.0.0.1")
	if err != nil || speed <= 0 {
		t.Fatalf("speed=%v err=%v", speed, err)
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


func TestMergeResultsPreservesHealthyAndFillsDeficit(t *testing.T) {
	m := NewManager()
	healthy := []Result{
		{IP:"1.1.1.1", Family:"ipv4", SpeedMB:60, LatencyMS:20, Qualified:true, SpeedTested:true},
		{IP:"1.1.1.2", Family:"ipv4", SpeedMB:55, LatencyMS:22, Qualified:true, SpeedTested:true},
		{IP:"1.1.1.3", Family:"ipv4", SpeedMB:50, LatencyMS:25, Qualified:true, SpeedTested:true},
	}
	supp := []Result{
		{IP:"1.1.1.2", Family:"ipv4", SpeedMB:80, LatencyMS:18, Qualified:true, SpeedTested:true}, // duplicate
		{IP:"1.1.1.4", Family:"ipv4", SpeedMB:70, LatencyMS:19, Qualified:true, SpeedTested:true},
		{IP:"1.1.1.5", Family:"ipv4", SpeedMB:65, LatencyMS:21, Qualified:true, SpeedTested:true},
		{IP:"1.1.1.6", Family:"ipv4", SpeedMB:90, LatencyMS:15, Qualified:false, SpeedTested:true}, // reject
	}
	got := m.MergeResults("s1", healthy, supp, 5)
	if len(got) != 5 {
		t.Fatalf("got %d results, want 5: %#v", len(got), got)
	}
	seen := map[string]bool{}
	for _, r := range got {
		if seen[r.IP] {
			t.Fatalf("duplicate IP %s", r.IP)
		}
		seen[r.IP] = true
	}
	for _, ip := range []string{"1.1.1.1","1.1.1.2","1.1.1.3","1.1.1.4","1.1.1.5"} {
		if !seen[ip] {
			t.Fatalf("missing %s in %#v", ip, got)
		}
	}
}


func TestPatchProgressCarriesPhaseTimer(t *testing.T) {
	m := NewManager()
	m.patchProgress("p", ScanProgress{Phase:"speed", Current:1, Total:4})
	time.Sleep(1100 * time.Millisecond)
	m.patchProgress("p", ScanProgress{Phase:"speed", Current:2, Total:4})
	p := m.Snapshot()["p"].Progress
	if p.Percent != 50 { t.Fatalf("percent=%d", p.Percent) }
	if p.StartedAt.IsZero() || p.ElapsedSeconds < 1 { t.Fatalf("timer not carried: %#v", p) }
	if p.ETASeconds < 1 { t.Fatalf("eta not computed: %#v", p) }
}
