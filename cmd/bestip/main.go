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
	app := server.New(store, eng)
	cfg := store.Get()
	if listen := os.Getenv("BESTIP_LISTEN"); listen != "" {
		cfg.Listen = listen
	}
	srv := &http.Server{Addr: cfg.Listen, Handler: app.Handler(), ReadHeaderTimeout: 10 * time.Second}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	go app.Scheduler(ctx)
	go func() {
		log.Printf("BestIP Manager %s (%s) listening on %s", buildinfo.Version, short(buildinfo.Commit), cfg.Listen)
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
