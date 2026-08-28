package engine

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
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
			Threads:   200,
			PingCount: 4,
			Port:      443,
			HTTPing:   true,
			ProbeURL:  "https://speed.cloudflare.com/cdn-cgi/trace",
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
		{IP: "1.1.1.1", Family: "ipv4", SpeedMB: 60, LatencyMS: 20, Qualified: true, SpeedTested: true},
		{IP: "1.1.1.2", Family: "ipv4", SpeedMB: 55, LatencyMS: 22, Qualified: true, SpeedTested: true},
		{IP: "1.1.1.3", Family: "ipv4", SpeedMB: 50, LatencyMS: 25, Qualified: true, SpeedTested: true},
	}
	supp := []Result{
		{IP: "1.1.1.2", Family: "ipv4", SpeedMB: 80, LatencyMS: 18, Qualified: true, SpeedTested: true}, // duplicate
		{IP: "1.1.1.4", Family: "ipv4", SpeedMB: 70, LatencyMS: 19, Qualified: true, SpeedTested: true},
		{IP: "1.1.1.5", Family: "ipv4", SpeedMB: 65, LatencyMS: 21, Qualified: true, SpeedTested: true},
		{IP: "1.1.1.6", Family: "ipv4", SpeedMB: 90, LatencyMS: 15, Qualified: false, SpeedTested: true}, // reject
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
	for _, ip := range []string{"1.1.1.1", "1.1.1.2", "1.1.1.3", "1.1.1.4", "1.1.1.5"} {
		if !seen[ip] {
			t.Fatalf("missing %s in %#v", ip, got)
		}
	}
}

func TestPatchProgressCarriesPhaseTimer(t *testing.T) {
	m := NewManager()
	m.patchProgress("p", ScanProgress{Phase: "speed", Current: 1, Total: 4})
	time.Sleep(1100 * time.Millisecond)
	m.patchProgress("p", ScanProgress{Phase: "speed", Current: 2, Total: 4})
	p := m.Snapshot()["p"].Progress
	if p.Percent != 50 {
		t.Fatalf("percent=%d", p.Percent)
	}
	if p.StartedAt.IsZero() || p.ElapsedSeconds < 1 {
		t.Fatalf("timer not carried: %#v", p)
	}
	if p.ETASeconds < 1 {
		t.Fatalf("eta not computed: %#v", p)
	}
}

func TestMergeResultsNeverEvictsHealthySurvivors(t *testing.T) {
	m := NewManager()
	healthy := []Result{
		{IP: "1.1.1.1", Qualified: true, SpeedMB: 31},
		{IP: "1.1.1.2", Qualified: true, SpeedMB: 32},
		{IP: "1.1.1.3", Qualified: true, SpeedMB: 33},
		{IP: "1.1.1.4", Qualified: true, SpeedMB: 34},
	}
	// New candidates are much faster, but health refill must only fill the one missing slot.
	supp := []Result{
		{IP: "2.2.2.1", Qualified: true, SpeedMB: 100},
		{IP: "2.2.2.2", Qualified: true, SpeedMB: 99},
		{IP: "2.2.2.3", Qualified: true, SpeedMB: 98},
	}
	got := m.MergeResults("s", healthy, supp, 5)
	if len(got) != 5 {
		t.Fatalf("len=%d", len(got))
	}
	seen := map[string]bool{}
	for _, r := range got {
		seen[r.IP] = true
	}
	for _, ip := range []string{"1.1.1.1", "1.1.1.2", "1.1.1.3", "1.1.1.4"} {
		if !seen[ip] {
			t.Fatalf("healthy survivor %s was evicted: %#v", ip, got)
		}
	}
	nNew := 0
	for _, r := range got {
		if strings.HasPrefix(r.IP, "2.2.2.") {
			nNew++
		}
	}
	if nNew != 1 {
		t.Fatalf("expected exactly 1 refill IP, got %d: %#v", nNew, got)
	}
}

func TestStopSourceCancelsActiveRun(t *testing.T) {
	m := NewManager()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runCtx, ok := m.beginRun(ctx, "s")
	if !ok {
		t.Fatal("beginRun failed")
	}
	done := make(chan struct{})
	go func() { <-runCtx.Done(); close(done) }()
	if !m.StopSource("s") {
		t.Fatal("StopSource returned false")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancel did not propagate")
	}
	m.endRun("s")
	if m.IsRunning("s") {
		t.Fatal("source still running")
	}
}

func TestNormalizeDecoratedIPInput(t *testing.T) {
	cases := []struct {
		raw, family, want string
	}{
		{"190.93.246.167:443#46.08MB/s-LAX", "ipv4", "190.93.246.167"},
		{"104.16.146.116:443#45.87MB/s-LAX", "ipv4", "104.16.146.116"},
		{"1.1.1.1#NRT", "ipv4", "1.1.1.1"},
		{"172.64.229.123/24#pool", "ipv4", "172.64.229.0/24"},
		{"[2606:4700:4700::1111]:443#NRT", "ipv6", "2606:4700:4700::1111"},
		{"2606:4700:5a::/48#NRT", "ipv6", "2606:4700:5a::/48"},
	}
	for _, tc := range cases {
		got, ok := normalizeInputEntry(tc.raw, tc.family)
		if !ok {
			t.Fatalf("failed to parse %q", tc.raw)
		}
		if got.Value != tc.want {
			t.Fatalf("%q => %q, want %q", tc.raw, got.Value, tc.want)
		}
	}
}

func TestCollectInputsDecoratedBestIPList(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "normalized.txt")
	s := config.Source{
		ID:     "decorated",
		Family: "ipv4",
		Inputs: []string{
			"190.93.246.167:443#46.08MB/s-LAX",
			"104.16.146.116:443#45.87MB/s-LAX",
			"104.16.123.147:443#45.87MB/s-LAX",
			"104.16.170.72:443#45.47MB/s-LAX",
		},
	}
	n, err := collectInputs(context.Background(), s, out)
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Fatalf("count=%d want 4", n)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Fields(string(b))
	want := []string{
		"seed:190.93.246.167",
		"seed:104.16.146.116",
		"seed:104.16.123.147",
		"seed:104.16.170.72",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalized=%#v want %#v", got, want)
	}
}

func TestNormalizeInputRejectsWrongFamily(t *testing.T) {
	if _, ok := normalizeInputEntry("190.93.246.167:443#LAX", "ipv6"); ok {
		t.Fatal("IPv4 accepted as IPv6")
	}
	if _, ok := normalizeInputEntry("[2606:4700::1111]:443#NRT", "ipv4"); ok {
		t.Fatal("IPv6 accepted as IPv4")
	}
}

func TestCollectInputsMarksManualSingleIPsAsSeeds(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "ranges.txt")
	s := config.Source{
		ID:     "seed-test",
		Family: "ipv4",
		Inputs: []string{
			"104.17.79.30",
			"104.17.79.0/24",
			"104.18.0.0/24",
		},
	}
	_, err := collectInputs(context.Background(), s, out)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(out)
	text := string(b)
	if !strings.Contains(text, "seed:104.17.79.30") {
		t.Fatalf("manual single IP was not marked seed: %s", text)
	}
	if strings.Contains(text, "seed:104.17.79.0/24") {
		t.Fatalf("CIDR must not become seed: %s", text)
	}
}
