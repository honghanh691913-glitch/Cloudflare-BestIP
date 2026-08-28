package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	DefaultProbeURL       = "https://speed.cloudflare.com/cdn-cgi/trace"
	DefaultSpeedURL       = "https://cf.090227.xyz/__down?bytes=99999999"
	DefaultRealTestURL    = "https://www.google.com/generate_204"
	DefaultMaxSampleCount = 10000
)

type Config struct {
	Version              int           `json:"version"`
	Listen               string        `json:"listen"`
	MaxConcurrency       int           `json:"max_concurrency"`
	ProbeURL             string        `json:"probe_url,omitempty"`
	SpeedURL             string        `json:"speed_url,omitempty"`
	HealthCheckMinutes   int           `json:"health_check_minutes,omitempty"`
	MaxSampleCount       int           `json:"max_sample_count,omitempty"`
	FurnaceRetentionDays int           `json:"furnace_retention_days,omitempty"`
	FurnaceAutoRank      bool          `json:"furnace_auto_rank"`
	RealTestURL          string        `json:"real_test_url,omitempty"`
	RealTestAttempts     int           `json:"real_test_attempts,omitempty"`
	RealSpeedURL         string        `json:"real_speed_url,omitempty"`
	RealSpeedBytesMB     int           `json:"real_speed_bytes_mb,omitempty"`
	RealSpeedTopN        int           `json:"real_speed_top_n,omitempty"`
	RealProfiles         []RealProfile `json:"real_profiles,omitempty"`
	Providers            []Provider    `json:"providers"`
	Sources              []Source      `json:"sources"`
	Targets              []Target      `json:"targets"`
	FurnaceRules         []FurnaceRule `json:"furnace_rules,omitempty"`
}

type Provider struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Type       string `json:"type"`
	ZoneID     string `json:"zone_id,omitempty"`
	ZoneDomain string `json:"zone_domain,omitempty"`

	// Cloudflare supports two authentication styles:
	// 1) API Token (recommended): Authorization: Bearer <token>
	// 2) Global API Key (legacy BestIP): X-Auth-Email + X-Auth-Key
	AuthMode string `json:"auth_mode,omitempty"` // api_token | global_api_key
	APIToken string `json:"api_token,omitempty"`
	Email    string `json:"email,omitempty"`
	APIKey   string `json:"api_key,omitempty"`
}

type RealProfile struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Protocol     string `json:"protocol"` // currently vless
	Server       string `json:"server"`   // template/original address; candidate IP replaces it during tests
	Port         int    `json:"port"`
	UUID         string `json:"uuid,omitempty"`
	Encryption   string `json:"encryption,omitempty"`
	Flow         string `json:"flow,omitempty"`
	Network      string `json:"network,omitempty"`  // ws / grpc / httpupgrade
	Security     string `json:"security,omitempty"` // tls / none
	SNI          string `json:"sni,omitempty"`
	Host         string `json:"host,omitempty"`
	Path         string `json:"path,omitempty"`
	Fingerprint  string `json:"fingerprint,omitempty"`
	ALPN         string `json:"alpn,omitempty"`
	Insecure     bool   `json:"insecure,omitempty"`
	ECH          string `json:"ech,omitempty"` // original URI field for round-trip/display
	ECHQueryName string `json:"ech_query_name,omitempty"`
	ECHDoH       string `json:"ech_doh,omitempty"`
	RawURI       string `json:"raw_uri,omitempty"`
}

type Source struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	Enabled         bool     `json:"enabled"`
	Family          string   `json:"family"` // ipv4 | ipv6
	Inputs          []string `json:"inputs"` // URL / file / CIDR / IP
	IntervalMinutes int      `json:"interval_minutes"`
	KeepResults     int      `json:"keep_results"`
	SampleCount     int      `json:"sample_count,omitempty"` // total candidates per run, distributed across ranges
	CFST            CFST     `json:"cfst"`

	// Optional real application-layer validation through a saved proxy profile.
	RealProfileID    string  `json:"real_profile_id,omitempty"`
	RealLatencyMaxMS float64 `json:"real_latency_max_ms,omitempty"`
	RealSpeedEnabled bool    `json:"real_speed_enabled,omitempty"`
	RealSpeedMinMB   float64 `json:"real_speed_min_mb,omitempty"`

	// Runtime-only global fallbacks populated by PrepareSource.
	GlobalProbeURL      string       `json:"-"`
	GlobalSpeedURL      string       `json:"-"`
	GlobalMaxSample     int          `json:"-"`
	GlobalRealTestURL   string       `json:"-"`
	GlobalRealAttempts  int          `json:"-"`
	GlobalRealSpeedURL  string       `json:"-"`
	GlobalRealSpeedMB   int          `json:"-"`
	GlobalRealSpeedTopN int          `json:"-"`
	RealProfile         *RealProfile `json:"-"`
}

type CFST struct {
	Binary        string   `json:"binary"`
	URL           string   `json:"url,omitempty"`       // optional per-line speed URL override
	ProbeURL      string   `json:"probe_url,omitempty"` // optional per-line trace/probe URL override
	Port          int      `json:"port"`
	Threads       int      `json:"threads"`
	PingCount     int      `json:"ping_count"`
	DownloadCount int      `json:"download_count"`
	DownloadTime  int      `json:"download_time"`
	LatencyMaxMS  float64  `json:"latency_max_ms"`
	LatencyMinMS  float64  `json:"latency_min_ms"`
	LossMax       float64  `json:"loss_max"`
	SpeedMinMB    float64  `json:"speed_min_mb"`
	HTTPing       bool     `json:"httping"`
	Colo          []string `json:"colo"`
	AllIP         bool     `json:"all_ip,omitempty"` // legacy only; v0.6 uses SampleCount
}

type Target struct {
	ID         string      `json:"id"`
	Name       string      `json:"name"` // optional note/remark
	Enabled    bool        `json:"enabled"`
	ProviderID string      `json:"provider_id"`
	Prefix     string      `json:"prefix,omitempty"`   // e.g. v4 / nrtv4 / @
	Hostname   string      `json:"hostname,omitempty"` // legacy/snapshot FQDN for backward compatibility
	TTL        int         `json:"ttl"`
	Proxied    bool        `json:"proxied"`
	Sources    []TargetRef `json:"sources"`
}

type TargetRef struct {
	SourceID string `json:"source_id"`
	Count    int    `json:"count"`
}

type FurnaceRule struct {
	SourceID     string  `json:"source_id"`
	Enabled      bool    `json:"enabled"`
	LatencyMaxMS float64 `json:"latency_max_ms"`
	LossMax      float64 `json:"loss_max"`
	SpeedMinMB   float64 `json:"speed_min_mb"`
	AutoRank     bool    `json:"auto_rank"`
}

type Store struct {
	path string
	mu   sync.RWMutex
	cfg  Config
}

func NewStore(path string) (*Store, error) {
	s := &Store{path: path}
	if err := s.Load(); err != nil {
		return nil, err
	}
	return s, nil
}

func ApplyDefaults(c *Config) {
	if c == nil {
		return
	}
	legacy := c.Version < 2
	if strings.TrimSpace(c.Listen) == "" {
		c.Listen = ":8080"
	}
	if c.MaxConcurrency < 1 {
		c.MaxConcurrency = 2
	}
	if strings.TrimSpace(c.ProbeURL) == "" {
		c.ProbeURL = DefaultProbeURL
	}
	if strings.TrimSpace(c.SpeedURL) == "" {
		c.SpeedURL = DefaultSpeedURL
	}
	if strings.TrimSpace(c.RealTestURL) == "" {
		c.RealTestURL = DefaultRealTestURL
	}
	if c.RealTestAttempts <= 0 {
		c.RealTestAttempts = 2
	}
	if c.RealTestAttempts > 5 {
		c.RealTestAttempts = 5
	}
	if strings.TrimSpace(c.RealSpeedURL) == "" {
		c.RealSpeedURL = c.SpeedURL
	}
	if c.RealSpeedBytesMB <= 0 {
		c.RealSpeedBytesMB = 5
	}
	if c.RealSpeedBytesMB > 100 {
		c.RealSpeedBytesMB = 100
	}
	if c.RealSpeedTopN <= 0 {
		c.RealSpeedTopN = 10
	}
	if c.RealSpeedTopN > 100 {
		c.RealSpeedTopN = 100
	}
	if c.HealthCheckMinutes < 0 {
		c.HealthCheckMinutes = 0
	}
	if legacy && c.HealthCheckMinutes == 0 {
		c.HealthCheckMinutes = 60
	}
	if c.MaxSampleCount <= 0 {
		c.MaxSampleCount = DefaultMaxSampleCount
	}
	if c.MaxSampleCount > 100000 {
		c.MaxSampleCount = 100000
	}
	if c.FurnaceRetentionDays <= 0 {
		c.FurnaceRetentionDays = 45
	}
	if legacy {
		c.FurnaceAutoRank = true
	}
	for i := range c.RealProfiles {
		p := &c.RealProfiles[i]
		p.Protocol = strings.ToLower(strings.TrimSpace(p.Protocol))
		if p.Protocol == "" {
			p.Protocol = "vless"
		}
		p.Network = strings.ToLower(strings.TrimSpace(p.Network))
		if p.Network == "" {
			p.Network = "ws"
		}
		p.Security = strings.ToLower(strings.TrimSpace(p.Security))
		if p.Security == "" {
			p.Security = "tls"
		}
		if p.Port <= 0 {
			p.Port = 443
		}
		if strings.TrimSpace(p.Path) == "" {
			p.Path = "/"
		}
		if strings.TrimSpace(p.Fingerprint) == "" && p.Security == "tls" {
			p.Fingerprint = "chrome"
		}
	}
	for i := range c.Sources {
		s := &c.Sources[i]
		if s.KeepResults <= 0 {
			s.KeepResults = 50
		}
		if s.SampleCount <= 0 {
			if s.CFST.AllIP && s.Family == "ipv4" {
				// Legacy All IP migration: for literal IPv4 CIDRs, preserve the
				// intuitive real count (/24 -> 256, two /24 -> 512). Remote lists
				// stay bounded by the global maximum and are clamped at runtime.
				s.SampleCount = legacyIPv4Capacity(s.Inputs, c.MaxSampleCount)
			} else {
				s.SampleCount = 256
			}
		}
		if s.SampleCount > c.MaxSampleCount {
			s.SampleCount = c.MaxSampleCount
		}
		s.CFST.AllIP = false
		if s.CFST.Port <= 0 {
			s.CFST.Port = 443
		}
		// Pre-v0.6 configs stored these two semantic names reversed while the
		// engine also emitted the reversed flags, so runtime behavior was still
		// usually -n 200 / -t 4. Migrate the common legacy shape once here.
		if legacy && s.CFST.Threads > 0 && s.CFST.Threads <= 16 && s.CFST.PingCount >= 32 {
			s.CFST.Threads, s.CFST.PingCount = s.CFST.PingCount, s.CFST.Threads
		}
		if s.CFST.Threads <= 0 {
			s.CFST.Threads = 200
		}
		if s.CFST.PingCount <= 0 {
			s.CFST.PingCount = 4
		}
		if s.CFST.DownloadTime <= 0 {
			s.CFST.DownloadTime = 10
		}
	}
	pruneOrphanTaskSources(c)
	c.Version = 2
}

// pruneOrphanTaskSources keeps the config aligned with the Web model where
// IP sources are owned/referenced by domain tasks. Editing a task used to be
// able to leave an old Source behind, which then appeared as a phantom furnace
// rule even though no task used it. Preserve standalone sources only when there
// are no tasks at all (legacy/manual configs).
func pruneOrphanTaskSources(c *Config) {
	if c == nil || len(c.Targets) == 0 {
		return
	}
	used := map[string]bool{}
	for _, t := range c.Targets {
		for _, ref := range t.Sources {
			if strings.TrimSpace(ref.SourceID) != "" {
				used[ref.SourceID] = true
			}
		}
	}
	keptSources := c.Sources[:0]
	for _, src := range c.Sources {
		if used[src.ID] {
			keptSources = append(keptSources, src)
		}
	}
	c.Sources = keptSources

	keptRules := c.FurnaceRules[:0]
	for _, rule := range c.FurnaceRules {
		if used[rule.SourceID] {
			keptRules = append(keptRules, rule)
		}
	}
	c.FurnaceRules = keptRules
}

func legacyIPv4Capacity(inputs []string, max int) int {
	if max <= 0 {
		max = DefaultMaxSampleCount
	}
	total := uint64(0)
	for _, raw := range inputs {
		v := strings.TrimSpace(raw)
		if v == "" {
			continue
		}
		if strings.HasPrefix(v, "http://") || strings.HasPrefix(v, "https://") {
			return max
		}
		if !strings.Contains(v, "/") {
			if net.ParseIP(v) == nil || net.ParseIP(v).To4() == nil {
				return max
			}
			total++
			continue
		}
		ip, n, err := net.ParseCIDR(v)
		if err != nil || ip.To4() == nil {
			return max
		}
		ones, bits := n.Mask.Size()
		if bits != 32 || ones < 0 {
			return max
		}
		cap := uint64(1) << uint(32-ones)
		total += cap
		if total >= uint64(max) {
			return max
		}
	}
	if total == 0 {
		return 256
	}
	return int(total)
}

func (s *Store) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return err
	}
	ApplyDefaults(&c)
	if err := Validate(c); err != nil {
		return err
	}
	s.cfg = c
	return nil
}

func (s *Store) Get() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, _ := json.Marshal(s.cfg)
	var out Config
	_ = json.Unmarshal(b, &out)
	ApplyDefaults(&out)
	return out
}

func (s *Store) Save(c Config) error {
	ApplyDefaults(&c)
	if err := Validate(c); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(s.path), 0755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	s.cfg = c
	return nil
}

func normalizeDomain(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	v = strings.TrimPrefix(v, "https://")
	v = strings.TrimPrefix(v, "http://")
	v = strings.Trim(v, " ./")
	if i := strings.IndexByte(v, '/'); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSuffix(v, ".")
}

func TargetHostname(p Provider, t Target) string {
	domain := normalizeDomain(p.ZoneDomain)
	prefix := strings.ToLower(strings.TrimSpace(t.Prefix))
	prefix = strings.Trim(prefix, " .")

	if domain != "" && prefix != "" {
		if prefix == "@" {
			return domain
		}
		if prefix == domain || strings.HasSuffix(prefix, "."+domain) {
			return prefix
		}
		return prefix + "." + domain
	}
	if domain != "" && strings.TrimSpace(t.Prefix) == "@" {
		return domain
	}
	return normalizeDomain(t.Hostname)
}

func CloudflareAuthMode(p Provider) string {
	mode := strings.ToLower(strings.TrimSpace(p.AuthMode))
	switch mode {
	case "global_api_key", "global", "api_key":
		return "global_api_key"
	case "api_token", "token":
		return "api_token"
	}
	if strings.TrimSpace(p.Email) != "" && strings.TrimSpace(p.APIKey) != "" && strings.TrimSpace(p.APIToken) == "" {
		return "global_api_key"
	}
	return "api_token"
}

func PrepareSource(c Config, s Source) Source {
	ApplyDefaults(&c)
	s.GlobalProbeURL = c.ProbeURL
	s.GlobalSpeedURL = c.SpeedURL
	s.GlobalMaxSample = c.MaxSampleCount
	s.GlobalRealTestURL = c.RealTestURL
	s.GlobalRealAttempts = c.RealTestAttempts
	s.GlobalRealSpeedURL = c.RealSpeedURL
	s.GlobalRealSpeedMB = c.RealSpeedBytesMB
	s.GlobalRealSpeedTopN = c.RealSpeedTopN
	if strings.TrimSpace(s.RealProfileID) != "" {
		for i := range c.RealProfiles {
			if c.RealProfiles[i].ID == s.RealProfileID {
				p := c.RealProfiles[i]
				s.RealProfile = &p
				break
			}
		}
	}
	if s.SampleCount <= 0 {
		s.SampleCount = 256
	}
	if s.GlobalMaxSample > 0 && s.SampleCount > s.GlobalMaxSample {
		s.SampleCount = s.GlobalMaxSample
	}
	return s
}

func FurnaceRuleFor(c Config, sourceID string) (FurnaceRule, bool) {
	for _, r := range c.FurnaceRules {
		if r.SourceID == sourceID {
			return r, true
		}
	}
	return FurnaceRule{}, false
}

func Validate(c Config) error {
	if c.Listen == "" {
		return errors.New("listen cannot be empty")
	}
	if c.MaxSampleCount < 1 || c.MaxSampleCount > 100000 {
		return fmt.Errorf("max_sample_count must be between 1 and 100000")
	}
	profileIDs := map[string]bool{}
	for _, p := range c.RealProfiles {
		if strings.TrimSpace(p.ID) == "" {
			return errors.New("real profile id cannot be empty")
		}
		if profileIDs[p.ID] {
			return fmt.Errorf("duplicate real profile id: %s", p.ID)
		}
		profileIDs[p.ID] = true
		if strings.ToLower(strings.TrimSpace(p.Protocol)) != "vless" {
			return fmt.Errorf("real profile %s: only vless is supported currently", p.ID)
		}
		if strings.TrimSpace(p.Server) == "" || p.Port < 1 || p.Port > 65535 || strings.TrimSpace(p.UUID) == "" {
			return fmt.Errorf("real profile %s: server, valid port and uuid are required", p.ID)
		}
		if n := strings.ToLower(strings.TrimSpace(p.Network)); n != "" && n != "ws" {
			return fmt.Errorf("real profile %s: v0.7 currently supports websocket transport", p.ID)
		}
	}
	sourceIDs := map[string]bool{}
	for _, s := range c.Sources {
		if s.ID == "" {
			return errors.New("source id cannot be empty")
		}
		if sourceIDs[s.ID] {
			return fmt.Errorf("duplicate source id: %s", s.ID)
		}
		sourceIDs[s.ID] = true
		if s.Family != "ipv4" && s.Family != "ipv6" {
			return fmt.Errorf("source %s: family must be ipv4 or ipv6", s.ID)
		}
		if strings.TrimSpace(s.RealProfileID) != "" && !profileIDs[s.RealProfileID] {
			return fmt.Errorf("source %s: real_profile_id %s not found", s.ID, s.RealProfileID)
		}
		if s.RealLatencyMaxMS < 0 || s.RealSpeedMinMB < 0 {
			return fmt.Errorf("source %s: real thresholds cannot be negative", s.ID)
		}
		if s.SampleCount < 1 {
			return fmt.Errorf("source %s: sample_count must be >= 1", s.ID)
		}
	}
	providerIDs := map[string]bool{}
	providers := map[string]Provider{}
	for _, p := range c.Providers {
		if p.ID == "" {
			return errors.New("provider id cannot be empty")
		}
		if providerIDs[p.ID] {
			return fmt.Errorf("duplicate provider id: %s", p.ID)
		}
		providerIDs[p.ID] = true
		providers[p.ID] = p
		if p.Type == "cloudflare" {
			if strings.TrimSpace(p.ZoneID) == "" {
				return fmt.Errorf("provider %s: zone_id cannot be empty", p.ID)
			}
			switch CloudflareAuthMode(p) {
			case "global_api_key":
				if strings.TrimSpace(p.Email) == "" || strings.TrimSpace(p.APIKey) == "" {
					return fmt.Errorf("provider %s: email and api_key are required for Global API Key auth", p.ID)
				}
			default:
				if strings.TrimSpace(p.APIToken) == "" {
					return fmt.Errorf("provider %s: api_token is required", p.ID)
				}
			}
		}
	}
	targetIDs := map[string]bool{}
	for _, t := range c.Targets {
		if t.ID == "" {
			return errors.New("target id cannot be empty")
		}
		if targetIDs[t.ID] {
			return fmt.Errorf("duplicate target id: %s", t.ID)
		}
		targetIDs[t.ID] = true
		p, ok := providers[t.ProviderID]
		if !ok {
			return fmt.Errorf("target %s references missing provider %s", t.ID, t.ProviderID)
		}
		if TargetHostname(p, t) == "" {
			return fmt.Errorf("target %s: prefix/hostname cannot be empty", t.ID)
		}
		for _, r := range t.Sources {
			if !sourceIDs[r.SourceID] {
				return fmt.Errorf("target %s references missing source %s", t.ID, r.SourceID)
			}
			if r.Count < 1 {
				return fmt.Errorf("target %s source %s count must be >= 1", t.ID, r.SourceID)
			}
		}
	}
	seenRules := map[string]bool{}
	for _, r := range c.FurnaceRules {
		if r.SourceID == "" || !sourceIDs[r.SourceID] {
			return fmt.Errorf("furnace rule references missing source %s", r.SourceID)
		}
		if seenRules[r.SourceID] {
			return fmt.Errorf("duplicate furnace rule for source %s", r.SourceID)
		}
		seenRules[r.SourceID] = true
		if r.LatencyMaxMS < 0 || r.LossMax < 0 || r.LossMax > 1 || r.SpeedMinMB < 0 {
			return fmt.Errorf("invalid furnace thresholds for source %s", r.SourceID)
		}
	}
	return nil
}
