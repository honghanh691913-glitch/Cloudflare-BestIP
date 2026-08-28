package reallink

import (
	"encoding/json"
	"strings"
	"testing"
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
