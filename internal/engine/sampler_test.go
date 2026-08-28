package engine

import (
	"bufio"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIPv6Three48Sample300(t *testing.T) {
	dir := t.TempDir()
	raw := filepath.Join(dir, "ranges.txt")
	out := filepath.Join(dir, "candidates.txt")
	content := "2606:4700:5a::/48\n2606:4700:52::/48\n2606:4700:57::/48\n"
	if err := os.WriteFile(raw, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	n, notes, err := sampleCandidateFile(raw, out, "ipv6", 300, 10000)
	if err != nil {
		t.Fatal(err)
	}
	if n != 300 {
		t.Fatalf("expected 300 candidates, got %d", n)
	}
	joined := strings.Join(notes, "|")
	for _, want := range []string{"2606:4700:5a::/48=100", "2606:4700:52::/48=100", "2606:4700:57::/48=100"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("allocation missing %q in %s", want, joined)
		}
	}
	_, a, _ := net.ParseCIDR("2606:4700:5a::/48")
	_, b, _ := net.ParseCIDR("2606:4700:52::/48")
	_, c, _ := net.ParseCIDR("2606:4700:57::/48")
	seen := map[string]bool{}
	f, _ := os.Open(out)
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		ip := net.ParseIP(strings.TrimSpace(sc.Text()))
		if ip == nil || ip.To4() != nil {
			t.Fatalf("invalid IPv6: %q", sc.Text())
		}
		if !a.Contains(ip) && !b.Contains(ip) && !c.Contains(ip) {
			t.Fatalf("IPv6 escaped source ranges: %s", ip)
		}
		if seen[ip.String()] {
			t.Fatalf("duplicate sampled IP: %s", ip)
		}
		seen[ip.String()] = true
	}
	if len(seen) != 300 {
		t.Fatalf("expected 300 unique IPs, got %d", len(seen))
	}
}

func TestIPv4Slash24ClampsTo256(t *testing.T) {
	dir := t.TempDir()
	raw := filepath.Join(dir, "ranges.txt")
	out := filepath.Join(dir, "candidates.txt")
	if err := os.WriteFile(raw, []byte("172.64.229.0/24\n"), 0600); err != nil {
		t.Fatal(err)
	}
	n, _, err := sampleCandidateFile(raw, out, "ipv4", 9999, 10000)
	if err != nil {
		t.Fatal(err)
	}
	if n != 256 {
		t.Fatalf("/24 must clamp to 256, got %d", n)
	}
}

func TestIPv4MultipleRangesEvenAllocation(t *testing.T) {
	dir := t.TempDir()
	raw := filepath.Join(dir, "ranges.txt")
	out := filepath.Join(dir, "candidates.txt")
	if err := os.WriteFile(raw, []byte("172.64.229.0/24\n172.64.230.0/24\n172.64.231.0/24\n"), 0600); err != nil {
		t.Fatal(err)
	}
	n, notes, err := sampleCandidateFile(raw, out, "ipv4", 300, 10000)
	if err != nil {
		t.Fatal(err)
	}
	if n != 300 {
		t.Fatalf("expected 300, got %d", n)
	}
	joined := strings.Join(notes, "|")
	if !strings.Contains(joined, "=100") {
		t.Fatalf("expected even 100-per-range allocation: %s", joined)
	}
}

func TestFixedSeedsAlwaysIncludedWithOverlappingCIDR(t *testing.T) {
	dir := t.TempDir()
	raw := filepath.Join(dir, "ranges.txt")
	out := filepath.Join(dir, "candidates.txt")
	content := strings.Join([]string{
		"seed:104.18.29.34",
		"seed:104.16.4.14",
		"seed:104.18.23.19",
		"seed:104.17.78.30",
		"seed:104.17.79.30",
		"104.17.79.0/24",
		"104.17.0.0/24",
		"104.18.0.0/24",
	}, "\n") + "\n"
	if err := os.WriteFile(raw, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	n, notes, err := sampleCandidateFile(raw, out, "ipv4", 256, 10000)
	if err != nil {
		t.Fatal(err)
	}
	if n != 256 {
		t.Fatalf("expected 256 candidates, got %d", n)
	}
	b, _ := os.ReadFile(out)
	seen := map[string]int{}
	for _, ip := range strings.Fields(string(b)) {
		seen[ip]++
	}
	for _, seed := range []string{"104.18.29.34", "104.16.4.14", "104.18.23.19", "104.17.78.30", "104.17.79.30"} {
		if seen[seed] != 1 {
			t.Fatalf("fixed seed %s count=%d, want exactly 1", seed, seen[seed])
		}
	}
	if !strings.Contains(strings.Join(notes, "|"), "固定种子=5") {
		t.Fatalf("notes missing fixed seed count: %#v", notes)
	}
}

func TestRequestedBelowSeedCountStillIncludesEverySeed(t *testing.T) {
	dir := t.TempDir()
	raw := filepath.Join(dir, "ranges.txt")
	out := filepath.Join(dir, "candidates.txt")
	if err := os.WriteFile(raw, []byte("seed:1.1.1.1\nseed:1.1.1.2\nseed:1.1.1.3\n"), 0600); err != nil {
		t.Fatal(err)
	}
	n, _, err := sampleCandidateFile(raw, out, "ipv4", 1, 10000)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("mandatory seeds must expand sample to 3, got %d", n)
	}
}
