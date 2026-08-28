package server

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/honghanh691913-glitch/Cloudflare-BestIP/internal/buildinfo"
	"github.com/honghanh691913-glitch/Cloudflare-BestIP/internal/config"
	dnsx "github.com/honghanh691913-glitch/Cloudflare-BestIP/internal/dns"
	"github.com/honghanh691913-glitch/Cloudflare-BestIP/internal/engine"
	"github.com/honghanh691913-glitch/Cloudflare-BestIP/internal/furnace"
	"github.com/honghanh691913-glitch/Cloudflare-BestIP/internal/updater"
)

//go:embed web/*
var webFS embed.FS

type App struct {
	Store   *config.Store
	Engine  *engine.Manager
	Furnace *furnace.Store
	dns     dnsx.CloudflareClient

	mu           sync.Mutex
	targetStatus map[string]any
	healthStatus map[string]any
	lastHealth   map[string]time.Time
	slots        chan struct{}
}

func New(store *config.Store, eng *engine.Manager, furnaceStore *furnace.Store) *App {
	max := store.Get().MaxConcurrency
	if max < 1 {
		max = 2
	}
	return &App{
		Store: store, Engine: eng, Furnace: furnaceStore,
		targetStatus: map[string]any{}, healthStatus: map[string]any{}, lastHealth: map[string]time.Time{},
		slots: make(chan struct{}, max),
	}
}

func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/config", a.configHandler)
	mux.HandleFunc("/api/status", a.statusHandler)
	mux.HandleFunc("/api/version", a.versionHandler)
	mux.HandleFunc("/api/update/check", a.updateCheckHandler)
	mux.HandleFunc("/api/update/apply", a.updateApplyHandler)
	mux.HandleFunc("/api/provider/test", a.providerTestHandler)
	mux.HandleFunc("/api/run/source", a.runSourceHandler)
	mux.HandleFunc("/api/run/all", a.runAllHandler)
	mux.HandleFunc("/api/health/source", a.healthSourceHandler)
	mux.HandleFunc("/api/sync/target", a.syncTargetHandler)
	mux.HandleFunc("/api/furnace", a.furnaceHandler)
	mux.HandleFunc("/api/furnace/detail", a.furnaceDetailHandler)
	sub, _ := fs.Sub(webFS, "web")
	mux.Handle("/", noCacheStatic(http.FileServer(http.FS(sub))))
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
	ts := cloneAnyMap(a.targetStatus)
	hs := cloneAnyMap(a.healthStatus)
	a.mu.Unlock()
	writeJSON(w, 200, map[string]any{
		"sources": a.Engine.Snapshot(),
		"targets": ts,
		"health":  hs,
		"build":   buildPayload(),
		"period":  furnace.PeriodName(time.Now()),
	})
}

func cloneAnyMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func buildPayload() map[string]any {
	return map[string]any{
		"version":          buildinfo.Version,
		"commit":           buildinfo.Commit,
		"built_at":         buildinfo.BuiltAt,
		"image":            buildinfo.Image,
		"repository":       buildinfo.Repository,
		"web_update_ready": updater.Available(),
	}
}

func (a *App) versionHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(405)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, buildPayload())
}

type githubCommit struct {
	SHA     string `json:"sha"`
	HTMLURL string `json:"html_url"`
}

func latestMainCommit(ctx context.Context) (githubCommit, error) {
	var out githubCommit
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/repos/"+buildinfo.Repository+"/commits/main", nil)
	if err != nil {
		return out, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "BestIP-Manager/"+buildinfo.Version)
	client := &http.Client{Timeout: 12 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return out, fmt.Errorf("GitHub 返回 %s", resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return out, err
	}
	if out.SHA == "" {
		return out, fmt.Errorf("GitHub 未返回 main 提交号")
	}
	return out, nil
}

func (a *App) updateCheckHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(405)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	latest, err := latestMainCommit(ctx)
	if err != nil {
		writeErr(w, 502, fmt.Errorf("检查更新失败: %w", err))
		return
	}
	current := strings.TrimSpace(buildinfo.Commit)
	available := current == "" || current == "unknown" || !strings.EqualFold(current, latest.SHA)
	writeJSON(w, 200, map[string]any{
		"current":          buildPayload(),
		"latest_commit":    latest.SHA,
		"latest_url":       latest.HTMLURL,
		"update_available": available,
	})
}

func (a *App) updateApplyHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(405)
		return
	}
	if !updater.Available() {
		writeErr(w, 409, fmt.Errorf("Web 一键更新未启用：请使用新版 Compose 挂载 /var/run/docker.sock"))
		return
	}
	target := os.Getenv("BESTIP_CONTAINER_NAME")
	if target == "" {
		target = "bestip-manager"
	}
	image := os.Getenv("BESTIP_UPDATE_IMAGE")
	if image == "" {
		image = buildinfo.Image
	}
	client, err := updater.NewClient()
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	if err := client.TriggerUpdate(ctx, target, image); err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 202, map[string]any{
		"ok":      true,
		"message": "已拉取最新版并启动更新助手，Web 将短暂断开后自动恢复",
		"image":   image,
	})
}

func (a *App) providerTestHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(405)
		return
	}
	var p config.Provider
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeErr(w, 400, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if err := a.dns.TestProvider(ctx, p); err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "message": "Cloudflare 认证和 Zone 访问正常"})
}

func (a *App) runSourceHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(405)
		return
	}
	id := r.URL.Query().Get("id")
	c := a.Store.Get()
	src, ok := sourceByID(c, id)
	if !ok {
		writeErr(w, 404, fmt.Errorf("source not found"))
		return
	}
	log.Printf("[api] manual scan requested source=%s name=%q", src.ID, src.Name)
	go a.runAndPublish(src)
	writeJSON(w, 202, map[string]any{"ok": true})
}

func (a *App) runAllHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(405)
		return
	}
	c := a.Store.Get()
	log.Printf("[api] run-all requested sources=%d", len(c.Sources))
	for _, s := range c.Sources {
		if s.Enabled {
			ss := s
			go a.runAndPublish(ss)
		}
	}
	writeJSON(w, 202, map[string]any{"ok": true})
}

func (a *App) healthSourceHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(405)
		return
	}
	id := r.URL.Query().Get("id")
	c := a.Store.Get()
	src, ok := sourceByID(c, id)
	if !ok {
		writeErr(w, 404, fmt.Errorf("source not found"))
		return
	}
	required := requiredCountForSource(c, id)
	if required < 1 {
		required = 1
	}
	if len(a.Engine.Latest(id)) < required {
		writeErr(w, 409, fmt.Errorf("当前没有足够的已选 IP 可做健康检查"))
		return
	}
	go a.runHealthCheck(config.PrepareSource(c, src), required, false)
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

func (a *App) furnaceHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(405)
		return
	}
	c := a.Store.Get()
	profiles := a.Furnace.Summaries(c, time.Now())
	if sourceID := strings.TrimSpace(r.URL.Query().Get("source_id")); sourceID != "" {
		filtered := profiles[:0]
		for _, p := range profiles {
			if p.SourceID == sourceID {
				filtered = append(filtered, p)
			}
		}
		profiles = filtered
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 1000
	}
	if len(profiles) > limit {
		profiles = profiles[:limit]
	}
	writeJSON(w, 200, map[string]any{
		"profiles": profiles,
		"period":   furnace.PeriodName(time.Now()),
		"rules":    c.FurnaceRules,
	})
}

func (a *App) furnaceDetailHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(405)
		return
	}
	sourceID := strings.TrimSpace(r.URL.Query().Get("source_id"))
	ip := strings.TrimSpace(r.URL.Query().Get("ip"))
	if sourceID == "" || ip == "" {
		writeErr(w, 400, fmt.Errorf("source_id and ip are required"))
		return
	}
	d, ok := a.Furnace.Detail(a.Store.Get(), sourceID, ip, time.Now())
	if !ok {
		writeErr(w, 404, fmt.Errorf("furnace profile not found"))
		return
	}
	writeJSON(w, 200, d)
}

func (a *App) runAndPublish(raw config.Source) {
	c := a.Store.Get()
	s, ok := sourceByID(c, raw.ID)
	if !ok || !s.Enabled {
		return
	}
	s = config.PrepareSource(c, s)
	log.Printf("[queue] source=%s waiting for slot", s.ID)
	a.slots <- struct{}{}
	defer func() { <-a.slots }()
	log.Printf("[queue] source=%s acquired slot", s.ID)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()
	started := time.Now()
	err := a.Engine.RunSource(ctx, s)
	a.ingestFurnace(c, s, a.Engine.History(s.ID))
	if err != nil {
		log.Printf("[scan] source=%s failed after %s: %v", s.ID, time.Since(started).Round(time.Millisecond), err)
		return
	}
	a.markHealthTime(s.ID, time.Now())
	log.Printf("[scan] source=%s completed after %s; checking DNS targets", s.ID, time.Since(started).Round(time.Millisecond))

	c = a.Store.Get()
	for _, t := range c.Targets {
		if !t.Enabled {
			continue
		}
		for _, ref := range t.Sources {
			if ref.SourceID == s.ID {
				if err := a.syncTarget(context.Background(), t.ID); err != nil {
					log.Printf("[dns] target=%s sync failed: %v", t.ID, err)
				}
				break
			}
		}
	}
}

func (a *App) ingestFurnace(c config.Config, s config.Source, rows []engine.Result) {
	rule, ok := config.FurnaceRuleFor(c, s.ID)
	if !ok || !rule.Enabled || len(rows) == 0 {
		return
	}
	if err := a.Furnace.Ingest(s.ID, s.Family, rows, rule, c.FurnaceRetentionDays); err != nil {
		log.Printf("[furnace] source=%s ingest failed: %v", s.ID, err)
		return
	}
	log.Printf("[furnace] source=%s ingested observations=%d", s.ID, len(rows))
}

func (a *App) runHealthCheck(s config.Source, required int, autoRescan bool) {
	if a.Engine.IsRunning(s.ID) {
		return
	}
	latest := a.Engine.Latest(s.ID)
	if len(latest) < required || required < 1 {
		return
	}
	current := append([]engine.Result(nil), latest[:required]...)
	log.Printf("[health] source=%s checking=%d thresholds latency<=%.0fms loss<=%.2f speed>=%.2fMB/s",
		s.ID, len(current), s.CFST.LatencyMaxMS, s.CFST.LossMax, s.CFST.SpeedMinMB)

	a.slots <- struct{}{}
	defer func() { <-a.slots }()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	report := a.Engine.CheckHealth(ctx, s, current)
	c := a.Store.Get()
	a.ingestFurnace(c, s, report.Rows)
	ok := report.Healthy >= required
	if ok {
		a.Engine.ApplyHealthyRefresh(s.ID, report.Rows)
	}
	a.mu.Lock()
	a.healthStatus[s.ID] = map[string]any{
		"time": time.Now(), "ok": ok, "checked": report.Checked, "healthy": report.Healthy, "required": required,
		"message": healthMessage(report, required),
	}
	a.lastHealth[s.ID] = time.Now()
	a.mu.Unlock()
	log.Printf("[health] source=%s checked=%d healthy=%d required=%d ok=%v", s.ID, report.Checked, report.Healthy, required, ok)
	if !ok && autoRescan && !a.Engine.IsRunning(s.ID) {
		log.Printf("[health] source=%s degraded; triggering full strict rescan", s.ID)
		go a.runAndPublish(s)
	}
}

func healthMessage(r engine.HealthReport, required int) string {
	if r.Healthy >= required {
		return fmt.Sprintf("%d/%d 个在用 IP 延迟、丢包、速度均达标", r.Healthy, required)
	}
	for _, row := range r.Rows {
		if !row.Qualified && row.RejectReason != "" {
			return row.RejectReason
		}
	}
	return fmt.Sprintf("仅 %d/%d 个在用 IP 仍达标", r.Healthy, required)
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
	if !t.Enabled {
		return fmt.Errorf("target disabled")
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
	hostname := config.TargetHostname(*p, *t)
	log.Printf("[dns] sync start target=%s host=%s auth=%s period=%s", t.ID, hostname, config.CloudflareAuthMode(*p), furnace.PeriodName(time.Now()))
	latest := map[string][]engine.Result{}
	for _, ref := range t.Sources {
		rows := a.Engine.Latest(ref.SourceID)
		rows = a.Furnace.Rank(c, ref.SourceID, rows, time.Now())
		latest[ref.SourceID] = rows
		if len(rows) < ref.Count {
			return fmt.Errorf("source %s only has %d ready results; target requires %d", ref.SourceID, len(rows), ref.Count)
		}
	}
	err := a.dns.SyncTarget(ctx, *p, *t, latest)
	if err != nil {
		log.Printf("[dns] sync failed target=%s host=%s: %v", t.ID, hostname, err)
	} else {
		log.Printf("[dns] sync success target=%s host=%s", t.ID, hostname)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	st := map[string]any{"time": time.Now(), "ok": err == nil, "period": furnace.PeriodName(time.Now())}
	if err != nil {
		st["error"] = err.Error()
	}
	a.targetStatus[id] = st
	return err
}

func (a *App) Scheduler(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	lastScan := map[string]time.Time{}
	lastPeriod := furnace.PeriodName(time.Now())
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c := a.Store.Get()
			now := time.Now()
			period := furnace.PeriodName(now)
			if c.FurnaceAutoRank && period != lastPeriod {
				log.Printf("[furnace] period changed %s -> %s; re-ranking enabled DNS targets", lastPeriod, period)
				lastPeriod = period
				for _, t := range c.Targets {
					if t.Enabled {
						tid := t.ID
						go func() { _ = a.syncTarget(context.Background(), tid) }()
					}
				}
			}

			for _, raw := range c.Sources {
				if !raw.Enabled {
					continue
				}
				s := config.PrepareSource(c, raw)
				lastRun := lastScan[s.ID]
				if st, ok := a.Engine.Snapshot()[s.ID]; ok && st.EndedAt.After(lastRun) {
					lastRun = st.EndedAt
					lastScan[s.ID] = lastRun
				}
				if s.IntervalMinutes > 0 && time.Since(lastRun) >= time.Duration(s.IntervalMinutes)*time.Minute && !a.Engine.IsRunning(s.ID) {
					lastScan[s.ID] = now
					ss := s
					log.Printf("[scheduler] strict scan trigger source=%s interval=%dm", s.ID, s.IntervalMinutes)
					go a.runAndPublish(ss)
					continue
				}
				if c.HealthCheckMinutes <= 0 || a.Engine.IsRunning(s.ID) {
					continue
				}
				required := requiredCountForSource(c, s.ID)
				if required < 1 || len(a.Engine.Latest(s.ID)) < required {
					continue
				}
				if time.Since(a.healthTime(s.ID)) >= time.Duration(c.HealthCheckMinutes)*time.Minute {
					a.markHealthTime(s.ID, now)
					ss := s
					go a.runHealthCheck(ss, required, true)
				}
			}
		}
	}
}

func (a *App) healthTime(id string) time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastHealth[id]
}

func (a *App) markHealthTime(id string, t time.Time) {
	a.mu.Lock()
	a.lastHealth[id] = t
	a.mu.Unlock()
}

func sourceByID(c config.Config, id string) (config.Source, bool) {
	for _, s := range c.Sources {
		if s.ID == id {
			return s, true
		}
	}
	return config.Source{}, false
}

func requiredCountForSource(c config.Config, sourceID string) int {
	max := 0
	for _, t := range c.Targets {
		if !t.Enabled {
			continue
		}
		for _, ref := range t.Sources {
			if ref.SourceID == sourceID && ref.Count > max {
				max = ref.Count
			}
		}
	}
	return max
}

func noCacheStatic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
		next.ServeHTTP(w, r)
	})
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
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]any{"error": strings.TrimSpace(err.Error())})
}
