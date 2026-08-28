package server

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

type UpdateState struct {
	Running   bool      `json:"running"`
	Stage     string    `json:"stage,omitempty"`
	Message   string    `json:"message,omitempty"`
	Error     string    `json:"error,omitempty"`
	StartedAt time.Time `json:"started_at,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

type App struct {
	Store   *config.Store
	Engine  *engine.Manager
	Furnace *furnace.Store
	dns     dnsx.CloudflareClient

	mu           sync.Mutex
	targetStatus map[string]any
	healthStatus map[string]any
	lastHealth   map[string]time.Time
	activeDNS    map[string][]engine.Result
	updateStatus UpdateState
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
		activeDNS: map[string][]engine.Result{},
		slots: make(chan struct{}, max),
	}
}

func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/config", a.configHandler)
	mux.HandleFunc("/api/status", a.statusHandler)
	mux.HandleFunc("/api/version", a.versionHandler)
	mux.HandleFunc("/api/update/check", a.updateCheckHandler)
	mux.HandleFunc("/api/update/status", a.updateStatusHandler)
	mux.HandleFunc("/api/update/apply", a.updateApplyHandler)
	mux.HandleFunc("/api/provider/test", a.providerTestHandler)
	mux.HandleFunc("/api/run/source", a.runSourceHandler)
	mux.HandleFunc("/api/stop/source", a.stopSourceHandler)
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
	active := cloneResultSlices(a.activeDNS)
	a.mu.Unlock()
	writeJSON(w, 200, map[string]any{
		"sources": a.Engine.Snapshot(),
		"targets": ts,
		"health":  hs,
		"active_dns": active,
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

func cloneResultSlices(in map[string][]engine.Result) map[string][]engine.Result {
	out := make(map[string][]engine.Result, len(in))
	for k, rows := range in {
		out[k] = append([]engine.Result(nil), rows...)
	}
	return out
}

func (a *App) setActiveDNS(sourceID string, rows []engine.Result) {
	a.mu.Lock()
	a.activeDNS[sourceID] = uniqueResultIPs(rows)
	a.mu.Unlock()
}

func (a *App) activeDNSFor(sourceID string) []engine.Result {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]engine.Result(nil), a.activeDNS[sourceID]...)
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
	client := &http.Client{Timeout: 6 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return out, fmt.Errorf("GitHub API 返回 %s", resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return out, err
	}
	if out.SHA == "" {
		return out, fmt.Errorf("GitHub API 未返回 main 提交号")
	}
	return out, nil
}

type remoteVersion struct {
	Version string
	Source  string
	URL     string
}

func fetchPlainVersion(ctx context.Context, rawURL, source string) (remoteVersion, error) {
	var out remoteVersion
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return out, err
	}
	req.Header.Set("User-Agent", "BestIP-Manager/"+buildinfo.Version)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return out, fmt.Errorf("%s 返回 %s", source, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 128))
	if err != nil {
		return out, err
	}
	v := strings.TrimSpace(string(b))
	if v == "" || len(v) > 64 || !strings.HasPrefix(strings.ToLower(v), "v") {
		return out, fmt.Errorf("%s VERSION 内容无效", source)
	}
	return remoteVersion{Version: v, Source: source, URL: rawURL}, nil
}

func latestVersionFallback(ctx context.Context) (remoteVersion, []string) {
	urls := []struct {
		url    string
		source string
	}{
		{"https://raw.githubusercontent.com/" + buildinfo.Repository + "/main/VERSION", "GitHub Raw"},
		{"https://cdn.jsdelivr.net/gh/" + buildinfo.Repository + "@main/VERSION", "jsDelivr"},
	}
	errs := []string{}
	for _, x := range urls {
		child, cancel := context.WithTimeout(ctx, 5*time.Second)
		v, err := fetchPlainVersion(child, x.url, x.source)
		cancel()
		if err == nil {
			return v, errs
		}
		errs = append(errs, x.source+": "+compactUpdateError(err))
	}
	return remoteVersion{}, errs
}

func compactUpdateError(err error) string {
	if err == nil {
		return ""
	}
	s := strings.TrimSpace(strings.ReplaceAll(err.Error(), "\n", " "))
	if len([]rune(s)) > 180 {
		r := []rune(s)
		s = string(r[:180]) + "…"
	}
	return s
}

func (a *App) updateCheckHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(405)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 18*time.Second)
	defer cancel()

	currentCommit := strings.TrimSpace(buildinfo.Commit)
	currentVersion := strings.TrimSpace(buildinfo.Version)

	apiCtx, apiCancel := context.WithTimeout(ctx, 6*time.Second)
	latest, apiErr := latestMainCommit(apiCtx)
	apiCancel()
	if apiErr == nil {
		available := currentCommit == "" || currentCommit == "unknown" || !strings.EqualFold(currentCommit, latest.SHA)
		writeJSON(w, 200, map[string]any{
			"current":          buildPayload(),
			"check_ok":         true,
			"check_source":     "GitHub API",
			"latest_commit":    latest.SHA,
			"latest_url":       latest.HTMLURL,
			"update_available": available,
			"can_force_update": updater.Available(),
		})
		return
	}

	fallback, fallbackErrs := latestVersionFallback(ctx)
	if fallback.Version != "" {
		available := currentVersion == "" || currentVersion == "dev" || currentVersion == "unknown" || !strings.EqualFold(currentVersion, fallback.Version)
		writeJSON(w, 200, map[string]any{
			"current":          buildPayload(),
			"check_ok":         true,
			"check_source":     fallback.Source,
			"latest_version":   fallback.Version,
			"latest_url":       fallback.URL,
			"update_available": available,
			"can_force_update": updater.Available(),
			"check_warning":    "GitHub API 不可达，已改用 VERSION 备用通道；Commit 无法精确比较",
			"api_error":        compactUpdateError(apiErr),
		})
		return
	}

	allErrs := append([]string{"GitHub API: " + compactUpdateError(apiErr)}, fallbackErrs...)
	writeJSON(w, 200, map[string]any{
		"current":          buildPayload(),
		"check_ok":         false,
		"check_source":     "unavailable",
		"update_available": false,
		"can_force_update": updater.Available(),
		"check_error":      strings.Join(allErrs, " | "),
	})
}

func (a *App) setUpdateState(st UpdateState) {
	a.mu.Lock()
	if st.UpdatedAt.IsZero() {
		st.UpdatedAt = time.Now()
	}
	a.updateStatus = st
	a.mu.Unlock()
}

func (a *App) patchUpdateState(stage, message, errText string, running bool) {
	a.mu.Lock()
	st := a.updateStatus
	if st.StartedAt.IsZero() {
		st.StartedAt = time.Now()
	}
	st.Running = running
	st.Stage = stage
	st.Message = message
	st.Error = errText
	st.UpdatedAt = time.Now()
	a.updateStatus = st
	a.mu.Unlock()
}

func (a *App) updateStatusHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(405)
		return
	}
	a.mu.Lock()
	st := a.updateStatus
	a.mu.Unlock()
	writeJSON(w, 200, st)
}

func (a *App) updateApplyHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(405)
		return
	}
	if !updater.Available() {
		writeErr(w, 409, fmt.Errorf("Web 一键更新未启用：请挂载 /var/run/docker.sock"))
		return
	}

	a.mu.Lock()
	if a.updateStatus.Running {
		st := a.updateStatus
		a.mu.Unlock()
		writeJSON(w, 202, map[string]any{
			"ok": true,
			"message": "更新任务已经在运行",
			"state": st,
		})
		return
	}
	now := time.Now()
	a.updateStatus = UpdateState{
		Running: true,
		Stage: "queued",
		Message: "更新请求已提交，准备连接 Docker Engine",
		StartedAt: now,
		UpdatedAt: now,
	}
	a.mu.Unlock()

	target := os.Getenv("BESTIP_CONTAINER_NAME")
	if target == "" {
		target = "bestip-manager"
	}
	image := os.Getenv("BESTIP_UPDATE_IMAGE")
	if image == "" {
		image = buildinfo.Image
	}

	log.Printf("[update] requested target=%s image=%s current=%s/%s",
		target, image, buildinfo.Version, buildinfo.Commit)

	go a.performWebUpdate(target, image)

	// Return immediately. Image pulling may take minutes on some NAS/ISP routes,
	// and the Web UI can now poll /api/update/status instead of appearing frozen.
	writeJSON(w, 202, map[string]any{
		"ok":      true,
		"message": "更新已进入后台；正在拉取 GHCR latest",
		"image":   image,
	})
}

func (a *App) performWebUpdate(target, image string) {
	a.patchUpdateState("docker", "正在连接 Docker Engine", "", true)
	log.Printf("[update] connecting docker engine")
	client, err := updater.NewClient()
	if err != nil {
		msg := compactUpdateError(err)
		a.patchUpdateState("failed", "连接 Docker Engine 失败", msg, false)
		log.Printf("[update] docker connection failed: %v", err)
		return
	}

	// TriggerUpdate first pulls the entire image, then creates/starts a helper
	// container. This is the slowest stage, so surface it explicitly.
	a.patchUpdateState("pulling", "正在从 GHCR 拉取 latest 镜像；首次或镜像较大时可能需要几分钟", "", true)
	log.Printf("[update] pulling image=%s", image)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	if err := client.TriggerUpdate(ctx, target, image); err != nil {
		msg := compactUpdateError(err)
		a.patchUpdateState("failed", "镜像拉取或更新助手启动失败", msg, false)
		log.Printf("[update] trigger failed: %v", err)
		return
	}

	// The helper sleeps briefly, then replaces this container. This state may
	// only be visible for 1-2 polls before the service disconnects, which is OK.
	a.patchUpdateState("restarting", "镜像已拉取，正在重建容器；页面即将短暂断开", "", true)
	log.Printf("[update] helper started; container restart imminent")
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

func (a *App) stopSourceHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(405)
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		writeErr(w, 400, fmt.Errorf("source id required"))
		return
	}
	if !a.Engine.StopSource(id) {
		writeJSON(w, 200, map[string]any{"ok": true, "running": false, "message": "任务已经停止"})
		return
	}
	a.mu.Lock()
	if h, ok := a.healthStatus[id].(map[string]any); ok {
		h["running"] = false
		h["phase"] = "stopping"
		h["message"] = "正在停止任务"
	}
	a.mu.Unlock()
	log.Printf("[api] stop requested source=%s", id)
	writeJSON(w, 202, map[string]any{"ok": true, "running": true, "message": "正在停止任务"})
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
	go a.runHealthCheck(config.PrepareSource(c, src), required, true)
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

func (a *App) runHealthCheck(s config.Source, required int, autoRefill bool) {
	if a.Engine.IsRunning(s.ID) {
		return
	}
	if required < 1 {
		return
	}
	c := a.Store.Get()
	current := a.activeDNSFor(s.ID)
	if len(current) < required {
		readCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		fresh, err := a.refreshActiveDNSForSource(readCtx, c, s.ID)
		cancel()
		if err != nil {
			a.mu.Lock()
			a.healthStatus[s.ID] = map[string]any{
				"time": time.Now(), "running": false, "phase": "read_failed", "ok": false,
				"required": required, "message": "读取 Cloudflare 当前 DNS IP 失败", "error": err.Error(),
			}
			a.mu.Unlock()
			log.Printf("[health] source=%s current DNS read failed: %v", s.ID, err)
			return
		}
		current = fresh
	}
	if len(current) > required {
		current = current[:required]
	}
	if len(current) == 0 {
		a.mu.Lock()
		a.healthStatus[s.ID] = map[string]any{
			"time": time.Now(), "running": false, "phase": "no_dns", "ok": false,
			"required": required, "message": "Cloudflare 当前没有可读取的在用 IP；不会因此自动全量严选",
		}
		a.mu.Unlock()
		return
	}
	log.Printf("[health] source=%s checking=%d thresholds latency<=%.0fms loss<=%.2f speed>=%.2fMB/s",
		s.ID, len(current), s.CFST.LatencyMaxMS, s.CFST.LossMax, s.CFST.SpeedMinMB)

	a.mu.Lock()
	a.healthStatus[s.ID] = map[string]any{
		"time": time.Now(), "running": true, "phase": "checking", "ok": false,
		"checked": 0, "healthy": 0, "required": required,
		"message": fmt.Sprintf("正在检查当前 DNS 的 %d 个 IP", required),
	}
	a.mu.Unlock()

	a.slots <- struct{}{}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	report := a.Engine.CheckHealth(ctx, s, current)
	cancel()
	<-a.slots

	c = a.Store.Get()
	a.ingestFurnace(c, s, report.Rows)
	a.setActiveDNS(s.ID, report.Rows)
	if report.Canceled {
		a.mu.Lock()
		a.healthStatus[s.ID] = map[string]any{
			"time": time.Now(), "running": false, "phase": "stopped", "ok": false,
			"checked": report.Checked, "required": required, "rows": report.Rows,
			"message": "健康检查已停止",
		}
		a.mu.Unlock()
		return
	}

	healthy := make([]engine.Result, 0, required)
	for _, row := range report.Rows {
		if row.Qualified {
			healthy = append(healthy, row)
		}
	}
	ok := len(healthy) >= required
	if ok {
		a.Engine.ApplyHealthyRefresh(s.ID, report.Rows)
	}
	a.mu.Lock()
	a.healthStatus[s.ID] = map[string]any{
		"time": time.Now(), "running": !ok && autoRefill, "phase": func() string { if ok { return "healthy" }; if autoRefill { return "refill" }; return "degraded" }(),
		"ok": ok, "checked": report.Checked, "healthy": len(healthy), "required": required,
		"need": maxServerInt(0, required-len(healthy)), "rows": report.Rows,
		"message": healthMessage(report, required),
	}
	a.lastHealth[s.ID] = time.Now()
	a.mu.Unlock()
	log.Printf("[health] source=%s checked=%d healthy=%d required=%d ok=%v", s.ID, report.Checked, len(healthy), required, ok)

	if ok || !autoRefill {
		return
	}
	need := required - len(healthy)
	log.Printf("[health] source=%s degraded; automatic refill need=%d", s.ID, need)
	if err := a.supplementSource(context.Background(), c, s, healthy, required); err != nil {
		if errors.Is(err, context.Canceled) {
			a.mu.Lock()
			a.healthStatus[s.ID] = map[string]any{
				"time": time.Now(), "running": false, "phase": "stopped", "ok": false,
				"checked": report.Checked, "healthy": len(healthy), "required": required, "need": need,
				"rows": report.Rows, "message": "自动补位已停止",
			}
			a.mu.Unlock()
			return
		}
		a.mu.Lock()
		a.healthStatus[s.ID] = map[string]any{
			"time": time.Now(), "running": false, "phase": "failed", "ok": false,
			"checked": report.Checked, "healthy": len(healthy), "required": required, "need": need,
			"rows": report.Rows, "error": err.Error(),
			"message": fmt.Sprintf("自动补位失败：%v", err),
		}
		a.mu.Unlock()
		log.Printf("[health] source=%s automatic refill failed: %v", s.ID, err)
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
	if err == nil {
		for _, ref := range t.Sources {
			rows := latest[ref.SourceID]
			if len(rows) > ref.Count {
				rows = rows[:ref.Count]
			}
			a.setActiveDNS(ref.SourceID, rows)
		}
	}
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


// BootstrapActiveDNS runs once after container start/update.
// It validates the IPs that are already published in DNS and only fills
// missing/unhealthy slots. A restart itself must never trigger a full strict scan.
func (a *App) BootstrapActiveDNS(ctx context.Context) {
	time.Sleep(2 * time.Second)
	c := a.Store.Get()
	log.Printf("[startup-health] begin targets=%d sources=%d", len(c.Targets), len(c.Sources))

	// Collect the currently-published IPs per source. Target refs are assigned
	// records of the matching family in declaration order.
	currentBySource := map[string][]engine.Result{}
	for _, t := range c.Targets {
		if !t.Enabled {
			continue
		}
		var p *config.Provider
		for i := range c.Providers {
			if c.Providers[i].ID == t.ProviderID {
				p = &c.Providers[i]
				break
			}
		}
		if p == nil {
			log.Printf("[startup-health] target=%s skipped: provider not found", t.ID)
			continue
		}
		readCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		recs, err := a.dns.ListTargetRecords(readCtx, *p, t)
		cancel()
		if err != nil {
			log.Printf("[startup-health] target=%s DNS read failed: %v", t.ID, err)
			continue
		}
		pos := map[string]int{"A": 0, "AAAA": 0}
		for _, ref := range t.Sources {
			s, ok := sourceByID(c, ref.SourceID)
			if !ok || ref.Count <= 0 {
				continue
			}
			typ := "A"
			if s.Family == "ipv6" {
				typ = "AAAA"
			}
			start := pos[typ]
			end := start + ref.Count
			if end > len(recs[typ]) {
				end = len(recs[typ])
			}
			for _, ip := range recs[typ][start:end] {
				currentBySource[s.ID] = append(currentBySource[s.ID], engine.Result{
					IP: ip, Family: s.Family, Qualified: true,
				})
			}
			pos[typ] = end
		}
	}

	for sourceID, rows := range currentBySource {
		a.setActiveDNS(sourceID, rows)
	}

	// Validate each enabled source's active set.
	for _, raw := range c.Sources {
		if !raw.Enabled {
			continue
		}
		s := config.PrepareSource(c, raw)
		required := requiredCountForSource(c, s.ID)
		if required < 1 {
			continue
		}
		current := uniqueResultIPs(currentBySource[s.ID])
		if len(current) > required {
			current = current[:required]
		}

		if len(current) > 0 {
			log.Printf("[startup-health] source=%s checking active DNS %d/%d", s.ID, len(current), required)
			a.slots <- struct{}{}
			checkCtx, cancel := context.WithTimeout(ctx, 20*time.Minute)
			report := a.Engine.CheckHealth(checkCtx, s, current)
			cancel()
			<-a.slots

			healthy := make([]engine.Result, 0, required)
			for _, r := range report.Rows {
				if r.Qualified {
					healthy = append(healthy, r)
				}
			}
			a.ingestFurnace(c, s, report.Rows)
			a.markHealthTime(s.ID, time.Now())

			a.mu.Lock()
			a.healthStatus[s.ID] = map[string]any{
				"time": time.Now(), "ok": len(healthy) >= required,
				"checked": report.Checked, "healthy": len(healthy), "required": required,
				"message": fmt.Sprintf("启动恢复：%d/%d 个当前 DNS IP 达标", len(healthy), required),
			}
			a.mu.Unlock()

			if len(healthy) >= required {
				a.Engine.SeedResults(s.ID, healthy[:required], "启动健康检查通过")
				log.Printf("[startup-health] source=%s all healthy=%d/%d; full scan skipped", s.ID, required, required)
				continue
			}

			need := required - len(healthy)
			log.Printf("[startup-health] source=%s degraded healthy=%d/%d need=%d; supplemental fill", s.ID, len(healthy), required, need)
			if err := a.supplementSource(ctx, c, s, healthy, required); err != nil {
				log.Printf("[startup-health] source=%s supplemental fill failed: %v", s.ID, err)
				// Keep healthy survivors in memory even if refill cannot finish.
				if len(healthy) > 0 {
					a.Engine.SeedResults(s.ID, healthy, "启动检查：等待补位")
				}
			}
			continue
		}

		// No active DNS record was readable. Do not immediately full-scan just
		// because the process restarted; wait for the normal interval or manual run.
		log.Printf("[startup-health] source=%s no active DNS records found; startup full scan skipped", s.ID)
	}
	log.Printf("[startup-health] complete")
}

func (a *App) refreshActiveDNSForSource(ctx context.Context, c config.Config, sourceID string) ([]engine.Result, error) {
	s, ok := sourceByID(c, sourceID)
	if !ok {
		return nil, fmt.Errorf("source not found")
	}
	out := []engine.Result{}
	var lastErr error
	for _, t := range c.Targets {
		if !t.Enabled {
			continue
		}
		uses := false
		for _, ref := range t.Sources {
			if ref.SourceID == sourceID {
				uses = true
				break
			}
		}
		if !uses {
			continue
		}
		var p *config.Provider
		for i := range c.Providers {
			if c.Providers[i].ID == t.ProviderID {
				p = &c.Providers[i]
				break
			}
		}
		if p == nil {
			continue
		}
		recs, err := a.dns.ListTargetRecords(ctx, *p, t)
		if err != nil {
			lastErr = err
			continue
		}
		typ := "A"
		if s.Family == "ipv6" {
			typ = "AAAA"
		}
		// If a target contains multiple sources of the same family, Cloudflare
		// records do not preserve source provenance. Use the configured slot order.
		pos := 0
		for _, ref := range t.Sources {
			rs, ok := sourceByID(c, ref.SourceID)
			if !ok || rs.Family != s.Family {
				continue
			}
			start := pos
			end := start + ref.Count
			if end > len(recs[typ]) {
				end = len(recs[typ])
			}
			if ref.SourceID == sourceID {
				for _, ip := range recs[typ][start:end] {
					out = append(out, engine.Result{IP: ip, Family: s.Family, Qualified: true})
				}
			}
			pos += ref.Count
		}
	}
	out = uniqueResultIPs(out)
	if len(out) > 0 {
		a.setActiveDNS(sourceID, out)
		return out, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return out, nil
}

func uniqueResultIPs(in []engine.Result) []engine.Result {
	seen := map[string]bool{}
	out := make([]engine.Result, 0, len(in))
	for _, r := range in {
		ip := strings.TrimSpace(r.IP)
		if ip == "" || seen[ip] {
			continue
		}
		seen[ip] = true
		out = append(out, r)
	}
	return out
}

// supplementSource preserves the currently healthy active set and searches only
// for the deficit. It starts with a small candidate sample and expands only when
// necessary, instead of jumping straight to the source's full 256/large sample.
func (a *App) supplementSource(ctx context.Context, c config.Config, s config.Source, healthy []engine.Result, required int) error {
	need := required - len(healthy)
	if need <= 0 {
		a.Engine.SeedResults(s.ID, healthy, "启动健康检查通过")
		return nil
	}
	full := s.SampleCount
	if full <= 0 {
		full = 256
	}
	a.mu.Lock()
	prevHealth, _ := a.healthStatus[s.ID].(map[string]any)
	a.healthStatus[s.ID] = map[string]any{
		"time": time.Now(), "running": true, "phase": "refill", "ok": false,
		"healthy": len(healthy), "required": required, "need": need,
		"rows": prevHealth["rows"], "message": fmt.Sprintf("当前 %d/%d 达标，正在自动补 %d 个", len(healthy), required, need),
	}
	a.mu.Unlock()
	sizes := []int{maxServerInt(32, need*24), maxServerInt(64, need*40), maxServerInt(128, need*64), full}
	tried := map[int]bool{}

	for _, n := range sizes {
		if n > full {
			n = full
		}
		if n < need {
			n = need
		}
		if tried[n] {
			continue
		}
		tried[n] = true

		ss := s
		ss.SampleCount = n
		ss.KeepResults = maxServerInt(need*3, need)
		log.Printf("[startup-health] source=%s supplement attempt sample=%d need=%d", s.ID, n, need)
		a.mu.Lock()
		if h, ok := a.healthStatus[s.ID].(map[string]any); ok {
			h["running"] = true; h["phase"] = "refill"; h["sample"] = n; h["attempt"] = len(tried);
			h["message"] = fmt.Sprintf("自动补位：缺 %d 个，本轮从 %d 个候选中寻找", need, n)
		}
		a.mu.Unlock()

		a.slots <- struct{}{}
		runCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
		err := a.Engine.RunSource(runCtx, ss)
		cancel()
		<-a.slots
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return err
			}
			log.Printf("[startup-health] source=%s supplement sample=%d failed: %v", s.ID, n, err)
			if n >= full {
				return err
			}
			continue
		}
		supplemental := a.Engine.Latest(s.ID)
		merged := a.Engine.MergeResults(s.ID, healthy, supplemental, required)
		if len(merged) >= required {
			a.ingestFurnace(c, s, a.Engine.History(s.ID))
			log.Printf("[startup-health] source=%s refill complete healthy=%d new=%d total=%d", s.ID, len(healthy), required-len(healthy), len(merged))
			a.mu.Lock()
			a.healthStatus[s.ID] = map[string]any{
				"time": time.Now(), "running": false, "phase": "healthy", "ok": true,
				"healthy": required, "required": required, "need": 0, "rows": merged,
				"message": fmt.Sprintf("自动补位完成：%d/%d 个在用 IP 达标", required, required),
			}
			a.mu.Unlock()
			// Publish only after a complete set is available.
			c2 := a.Store.Get()
			for _, t := range c2.Targets {
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
			return nil
		}
		if n >= full {
			break
		}
	}
	return fmt.Errorf("补位未找到足够达标 IP：当前 %d/%d", len(a.Engine.Latest(s.ID)), required)
}

func maxServerInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func (a *App) Scheduler(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	// A process/container restart is not a reason to perform a full strict scan.
	// Start the periodic interval from this process start; BootstrapActiveDNS
	// separately validates the currently published IPs and fills only deficits.
	lastScan := map[string]time.Time{}
	startedAt := time.Now()
	for _, s := range a.Store.Get().Sources {
		if s.Enabled {
			lastScan[s.ID] = startedAt
		}
	}
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
				if required < 1 {
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
