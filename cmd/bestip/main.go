package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/yourname/bestip-manager/internal/config"
	"github.com/yourname/bestip-manager/internal/engine"
	"github.com/yourname/bestip-manager/internal/server"
)

func main() {
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
	srv := &http.Server{Addr: cfg.Listen, Handler: app.Handler(), ReadHeaderTimeout: 10 * time.Second}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	go app.Scheduler(ctx)
	go func() {
		log.Printf("BestIP Manager listening on %s", cfg.Listen)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()
	<-ctx.Done()
	shutdown, _ := context.WithTimeout(context.Background(), 10*time.Second)
	_ = srv.Shutdown(shutdown)
}
