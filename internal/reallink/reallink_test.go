package reallink

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParseUserVLESSURI(t *testing.T) {
	raw := "vless://a8e1bbcd-aae2-41f4-810f-ac9f20860356@172.64.229.36:443?encryption=none&security=tls&sni=us.755gaoyi.cc.cd&fp=chrome&insecure=0&allowInsecure=0&ech=cloudflare-ech.com%2Bhttps%3A%2F%2Fdns.alidns.com%2Fdns-query&type=ws&host=us.755gaoyi.cc.cd&path=%2F#NRT-07"
	p, err := ParseURI(raw)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "NRT-07" || p.Server != "172.64.229.36" || p.Port != 443 {
		t.Fatalf("basic parse: %#v", p)
	}
	if p.UUID != "a8e1bbcd-aae2-41f4-810f-ac9f20860356" || p.SNI != "us.755gaoyi.cc.cd" || p.Host != "us.755gaoyi.cc.cd" {
		t.Fatalf("vless fields: %#v", p)
	}
	if p.Network != "ws" || p.Path != "/" || p.Fingerprint != "chrome" || p.Insecure {
		t.Fatalf("transport/tls: %#v", p)
	}
	if p.ECHQueryName != "cloudflare-ech.com" || p.ECHDoH != "https://dns.alidns.com/dns-query" {
		t.Fatalf("ECH parse: %#v", p)
	}
}

func TestBuildConfigOverridesOnlyCandidateAddress(t *testing.T) {
	raw := "vless://a8e1bbcd-aae2-41f4-810f-ac9f20860356@172.64.229.36:443?security=tls&sni=us.755gaoyi.cc.cd&fp=chrome&ech=cloudflare-ech.com%2Bhttps%3A%2F%2Fdns.alidns.com%2Fdns-query&type=ws&host=us.755gaoyi.cc.cd&path=%2F#NRT-07"
	p, _ := ParseURI(raw)
	cfg, err := BuildConfig(p, "104.16.1.2", 32123)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(cfg)
	s := string(b)
	for _, want := range []string{
		`"server":"104.16.1.2"`,
		`"server_port":443`,
		`"server_name":"us.755gaoyi.cc.cd"`,
		`"Host":"us.755gaoyi.cc.cd"`,
		`"query_server_name":"cloudflare-ech.com"`,
		`"server":"dns.alidns.com"`,
		`"listen_port":32123`,
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %s in %s", want, s)
		}
	}
}

func TestBuildBatchConfigRoutesEachInboundToMatchingCandidate(t *testing.T) {
	raw := "vless://a8e1bbcd-aae2-41f4-810f-ac9f20860356@172.64.229.36:443?security=tls&sni=us.755gaoyi.cc.cd&fp=chrome&ech=cloudflare-ech.com%2Bhttps%3A%2F%2Fdns.alidns.com%2Fdns-query&type=ws&host=us.755gaoyi.cc.cd&path=%2F#NRT-07"
	p, err := ParseURI(raw)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := BuildBatchConfig(p, []string{"172.64.229.36", "104.16.1.2", "104.16.2.3"}, []int{31001, 31002, 31003})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(cfg)
	s := string(b)
	for _, want := range []string{
		`"tag":"test-out-0"`, `"server":"172.64.229.36"`,
		`"tag":"test-out-1"`, `"server":"104.16.1.2"`,
		`"tag":"test-out-2"`, `"server":"104.16.2.3"`,
		`"inbound":["mixed-1"]`, `"outbound":"test-out-1"`,
		`"query_server_name":"cloudflare-ech.com"`,
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %s in %s", want, s)
		}
	}
}

func TestV073RealDelayUsesMinLikeV2rayN(t *testing.T) {
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		if n == 1 {
			time.Sleep(80 * time.Millisecond)
		} else {
			time.Sleep(10 * time.Millisecond)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	tr := &http.Transport{IdleConnTimeout: 5 * time.Second, MaxIdleConnsPerHost: 2}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: time.Second}

	values := make([]float64, 0, 2)
	for i := 0; i < 2; i++ {
		start := time.Now()
		if err := doLatencyRequestWithClient(context.Background(), client, srv.URL); err != nil {
			t.Fatal(err)
		}
		values = append(values, float64(time.Since(start).Microseconds())/1000)
	}
	sort.Float64s(values)
	if values[0] >= values[1] {
		t.Fatalf("expected min/second sample to be faster, got %#v", values)
	}
	if values[0] > 60 {
		t.Fatalf("minimum sample unexpectedly high: %#v", values)
	}
}

func TestV075SpeedWarmupURL(t *testing.T) {
	raw := "https://cf.090227.xyz/__down?bytes=99999999"
	got, ok := speedWarmupURL(raw, 262144)
	if !ok {
		t.Fatal("expected bytes endpoint to support warmup")
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	if u.Query().Get("bytes") != strconv.Itoa(262144) {
		t.Fatalf("warmup bytes=%q", u.Query().Get("bytes"))
	}
}

func TestV075GenericSpeedURLDoesNotInventBytes(t *testing.T) {
	if got, ok := speedWarmupURL("https://example.com/file.bin", 262144); ok || got != "" {
		t.Fatalf("unexpected warmup rewrite: %q %v", got, ok)
	}
}
