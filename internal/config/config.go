package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

type Config struct {
	Version        int        `json:"version"`
	Listen         string     `json:"listen"`
	MaxConcurrency int        `json:"max_concurrency"`
	Providers      []Provider `json:"providers"`
	Sources        []Source   `json:"sources"`
	Targets        []Target   `json:"targets"`
}

type Provider struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Type     string `json:"type"`
	ZoneID   string `json:"zone_id,omitempty"`
	APIToken string `json:"api_token,omitempty"`
}

type Source struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	Enabled         bool     `json:"enabled"`
	Family          string   `json:"family"` // ipv4 | ipv6
	Inputs          []string `json:"inputs"` // URL / file / CIDR / IP
	IntervalMinutes int      `json:"interval_minutes"`
	KeepResults     int      `json:"keep_results"`
	CFST            CFST     `json:"cfst"`
}

type CFST struct {
	Binary        string   `json:"binary"`
	URL           string   `json:"url"`
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
	AllIP         bool     `json:"all_ip"`
}

type Target struct {
	ID         string      `json:"id"`
	Name       string      `json:"name"`
	Enabled    bool        `json:"enabled"`
	ProviderID string      `json:"provider_id"`
	Hostname   string      `json:"hostname"`
	TTL        int         `json:"ttl"`
	Proxied    bool        `json:"proxied"`
	Sources    []TargetRef `json:"sources"`
}

type TargetRef struct {
	SourceID string `json:"source_id"`
	Count    int    `json:"count"`
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
	return out
}

func (s *Store) Save(c Config) error {
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

func Validate(c Config) error {
	if c.Listen == "" {
		return errors.New("listen cannot be empty")
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
	}
	providerIDs := map[string]bool{}
	for _, p := range c.Providers {
		providerIDs[p.ID] = true
	}
	targetIDs := map[string]bool{}
	for _, t := range c.Targets {
		if t.ID == "" || t.Hostname == "" {
			return errors.New("target id/hostname cannot be empty")
		}
		if targetIDs[t.ID] {
			return fmt.Errorf("duplicate target id: %s", t.ID)
		}
		targetIDs[t.ID] = true
		if !providerIDs[t.ProviderID] {
			return fmt.Errorf("target %s references missing provider %s", t.ID, t.ProviderID)
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
	return nil
}
