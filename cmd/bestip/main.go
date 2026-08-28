package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/honghanh691913-glitch/Cloudflare-BestIP/internal/buildinfo"
	"github.com/honghanh691913-glitch/Cloudflare-BestIP/internal/config"
	"github.com/honghanh691913-glitch/Cloudflare-BestIP/internal/engine"
	"github.com/honghanh691913-glitch/Cloudflare-BestIP/internal/furnace"
	"github.com/honghanh691913-glitch/Cloudflare-BestIP/internal/server"
	"github.com/honghanh691913-glitch/Cloudflare-BestIP/internal/updater"
)

func main() {
	if os.Getenv("BESTIP_UPDATE_HELPER") == "1" {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		if err := updater.RunHelper(ctx); err != nil {
			log.Printf("BestIP update helper failed: %v", err)
			os.Exit(1)
		}
		return
	}

	cfgPath := os.Getenv("BESTIP_CONFIG")
	if cfgPath == "" {
		cfgPath = "/data/config.json"
	}
	store, err := config.NewStore(cfgPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	eng := engine.NewManager()
	furnaceStore, err := furnace.New(furnace.DefaultPath(cfgPath))
	if err != nil {
		log.Fatalf("furnace: %v", err)
	}
	app := server.New(store, eng, furnaceStore)
	cfg := store.Get()
	if listen := os.Getenv("BESTIP_LISTEN"); listen != "" {
		cfg.Listen = listen
	}
	srv := &http.Server{Addr: cfg.Listen, Handler: app.Handler(), ReadHeaderTimeout: 10 * time.Second}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	log.Printf("[startup] BestIP Manager version=%s commit=%s listen=%s config=%s sources=%d targets=%d providers=%d concurrency=%d web_update=%v",
		buildinfo.Version, short(buildinfo.Commit), cfg.Listen, cfgPath, len(cfg.Sources), len(cfg.Targets), len(cfg.Providers), cfg.MaxConcurrency, updater.Available())
	go app.BootstrapActiveDNS(ctx)
	go app.Scheduler(ctx)
	go func() {
		log.Printf("[startup] HTTP server listening on %s", cfg.Listen)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()
	<-ctx.Done()
	shutdown, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelShutdown()
	_ = srv.Shutdown(shutdown)
}

func short(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}
