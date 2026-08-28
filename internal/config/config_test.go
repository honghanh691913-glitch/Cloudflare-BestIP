package config

import "testing"

func TestTargetHostnameFromPrefix(t *testing.T) {
	p := Provider{ZoneDomain: "629717.xyz"}
	cases := []struct {
		prefix string
		want   string
	}{
		{"v4", "v4.629717.xyz"},
		{"nrt.v4", "nrt.v4.629717.xyz"},
		{"@", "629717.xyz"},
		{"V4.629717.XYZ", "v4.629717.xyz"},
	}
	for _, tc := range cases {
		got := TargetHostname(p, Target{Prefix: tc.prefix})
		if got != tc.want {
			t.Fatalf("prefix %q: got %q want %q", tc.prefix, got, tc.want)
		}
	}
}

func TestTargetHostnameLegacy(t *testing.T) {
	got := TargetHostname(Provider{}, Target{Hostname: "V4.629717.XYZ."})
	if got != "v4.629717.xyz" {
		t.Fatalf("legacy hostname = %q", got)
	}
}

func TestCloudflareAuthMode(t *testing.T) {
	if got := CloudflareAuthMode(Provider{APIToken: "token"}); got != "api_token" {
		t.Fatalf("token mode = %s", got)
	}
	if got := CloudflareAuthMode(Provider{Email: "a@b.com", APIKey: "key"}); got != "global_api_key" {
		t.Fatalf("legacy mode = %s", got)
	}
}

func TestApplyDefaultsMigratesLegacyThreadPingFields(t *testing.T) {
	c := Config{Listen: ":8080", MaxSampleCount: 10000, Sources: []Source{{ID: "s", Family: "ipv4", SampleCount: 256, CFST: CFST{Threads: 4, PingCount: 200}}}}
	ApplyDefaults(&c)
	if c.Sources[0].CFST.Threads != 200 || c.Sources[0].CFST.PingCount != 4 {
		t.Fatalf("legacy migration failed: threads=%d ping_count=%d", c.Sources[0].CFST.Threads, c.Sources[0].CFST.PingCount)
	}
}

func TestApplyDefaultsPrunesOrphanTaskSources(t *testing.T) {
	c := Config{
		Listen: ":8080", MaxSampleCount: 10000,
		Sources: []Source{
			{ID: "v4", Family: "ipv4", SampleCount: 256},
			{ID: "v6", Family: "ipv6", SampleCount: 256},
			{ID: "old-v6", Family: "ipv6", SampleCount: 256},
		},
		Targets: []Target{
			{ID: "t4", Sources: []TargetRef{{SourceID: "v4", Count: 5}}},
			{ID: "t6", Sources: []TargetRef{{SourceID: "v6", Count: 5}}},
		},
		FurnaceRules: []FurnaceRule{
			{SourceID: "v4", Enabled: true},
			{SourceID: "v6", Enabled: true},
			{SourceID: "old-v6", Enabled: true},
		},
	}
	ApplyDefaults(&c)
	if len(c.Sources) != 2 {
		t.Fatalf("sources=%d want 2: %#v", len(c.Sources), c.Sources)
	}
	if len(c.FurnaceRules) != 2 {
		t.Fatalf("rules=%d want 2: %#v", len(c.FurnaceRules), c.FurnaceRules)
	}
}

func TestApplyDefaultsKeepsStandaloneSourcesWhenNoTasks(t *testing.T) {
	c := Config{Listen: ":8080", MaxSampleCount: 10000, Sources: []Source{{ID: "standalone", Family: "ipv4", SampleCount: 256}}}
	ApplyDefaults(&c)
	if len(c.Sources) != 1 || c.Sources[0].ID != "standalone" {
		t.Fatalf("standalone source unexpectedly pruned: %#v", c.Sources)
	}
}
