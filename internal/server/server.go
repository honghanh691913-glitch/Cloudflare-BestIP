package server

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/yourname/bestip-manager/internal/config"
	dnsx "github.com/yourname/bestip-manager/internal/dns"
	"github.com/yourname/bestip-manager/internal/engine"
)

//go:embed web/*
var webFS embed.FS

type App struct {
	Store        *config.Store
	Engine       *engine.Manager
	dns          dnsx.CloudflareClient
	mu           sync.Mutex
	targetStatus map[string]any
}

func New(store *config.Store, eng *engine.Manager) *App {
	return &App{Store: store, Engine: eng, targetStatus: map[string]any{}}
}

func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/config", a.configHandler)
	mux.HandleFunc("/api/status", a.statusHandler)
	mux.HandleFunc("/api/run/source", a.runSourceHandler)
	mux.HandleFunc("/api/run/all", a.runAllHandler)
	mux.HandleFunc("/api/sync/target", a.syncTargetHandler)
	sub, _ := fs.Sub(webFS, "web")
	mux.Handle("/", http.FileServer(http.FS(sub)))
	return basicAuth(mux)
}

func (a *App) configHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, a.Store.Get())
	case http.MethodPut:
		var c config.Config
		if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
			writeErr(w, 400, err)
			return
		}
		if err := a.Store.Save(c); err != nil {
			writeErr(w, 400, err)
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	default:
		w.WriteHeader(405)
	}
}
func (a *App) statusHandler(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	ts := a.targetStatus
	a.mu.Unlock()
	writeJSON(w, 200, map[string]any{"sources": a.Engine.Snapshot(), "targets": ts})
}
func (a *App) runSourceHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(405)
		return
	}
	id := r.URL.Query().Get("id")
	var src *config.Source
	c := a.Store.Get()
	for i := range c.Sources {
		if c.Sources[i].ID == id {
			src = &c.Sources[i]
			break
		}
	}
	if src == nil {
		writeErr(w, 404, fmt.Errorf("source not found"))
		return
	}
	go a.runAndPublish(*src)
	writeJSON(w, 202, map[string]any{"ok": true})
}
func (a *App) runAllHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(405)
		return
	}
	c := a.Store.Get()
	for _, s := range c.Sources {
		if s.Enabled {
			ss := s
			go a.runAndPublish(ss)
		}
	}
	writeJSON(w, 202, map[string]any{"ok": true})
}
func (a *App) syncTargetHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(405)
		return
	}
	id := r.URL.Query().Get("id")
	if err := a.syncTarget(context.Background(), id); err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (a *App) runAndPublish(s config.Source) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()
	if err := a.Engine.RunSource(ctx, s); err != nil {
		return
	}
	c := a.Store.Get()
	for _, t := range c.Targets {
		if !t.Enabled {
			continue
		}
		for _, ref := range t.Sources {
			if ref.SourceID == s.ID {
				_ = a.syncTarget(context.Background(), t.ID)
				break
			}
		}
	}
}
func (a *App) syncTarget(ctx context.Context, id string) error {
	c := a.Store.Get()
	var t *config.Target
	for i := range c.Targets {
		if c.Targets[i].ID == id {
			t = &c.Targets[i]
			break
		}
	}
	if t == nil {
		return fmt.Errorf("target not found")
	}
	var p *config.Provider
	for i := range c.Providers {
		if c.Providers[i].ID == t.ProviderID {
			p = &c.Providers[i]
			break
		}
	}
	if p == nil {
		return fmt.Errorf("provider not found")
	}
	latest := map[string][]engine.Result{}
	for _, r := range t.Sources {
		latest[r.SourceID] = a.Engine.Latest(r.SourceID)
		if len(latest[r.SourceID]) == 0 {
			return fmt.Errorf("source %s has no results yet", r.SourceID)
		}
	}
	err := a.dns.SyncTarget(ctx, *p, *t, latest)
	a.mu.Lock()
	defer a.mu.Unlock()
	st := map[string]any{"time": time.Now(), "ok": err == nil}
	if err != nil {
		st["error"] = err.Error()
	}
	a.targetStatus[id] = st
	return err
}

func (a *App) Scheduler(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	last := map[string]time.Time{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c := a.Store.Get()
			for _, s := range c.Sources {
				if !s.Enabled || s.IntervalMinutes <= 0 {
					continue
				}
				if time.Since(last[s.ID]) >= time.Duration(s.IntervalMinutes)*time.Minute {
					last[s.ID] = time.Now()
					ss := s
					go a.runAndPublish(ss)
				}
			}
		}
	}
}

func basicAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u := os.Getenv("BESTIP_WEB_USER")
		p := os.Getenv("BESTIP_WEB_PASS")
		if u == "" && p == "" {
			next.ServeHTTP(w, r)
			return
		}
		ru, rp, ok := r.BasicAuth()
		if !ok || ru != u || rp != p {
			w.Header().Set("WWW-Authenticate", `Basic realm="BestIP"`)
			http.Error(w, "Unauthorized", 401)
			return
		}
		next.ServeHTTP(w, r)
	})
}
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]any{"error": strings.TrimSpace(err.Error())})
}
