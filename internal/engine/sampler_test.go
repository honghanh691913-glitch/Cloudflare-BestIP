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
