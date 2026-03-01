package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	if os.Geteuid() != 0 {
		log.Fatalf("this server must run as root (try: sudo go run ./cmd/server)")
	}

	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	if err := ensurePreflight(cfg); err != nil {
		log.Fatalf("preflight failed: %v", err)
	}
	logStartupDiagnostics(cfg)

	srv := &server{
		cfg:       cfg,
		sandboxes: make(map[string]*sandbox),
	}

	// Install one broad NAT rule once; keep it for server lifetime.
	if err := ensureGlobalMasquerade(cfg.HostNATIface); err != nil {
		log.Fatalf("ensure global MASQUERADE: %v", err)
	}

	// Initialize netns pool if enabled.
	if cfg.NetnsPoolSize > 0 {
		srv.netnsPool = newNetnsPool(cfg, cfg.NetnsPoolSize)
		if err := srv.netnsPool.Init(); err != nil {
			log.Fatalf("init netns pool: %v", err)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /create", srv.handleCreate)
	mux.HandleFunc("POST /exec", srv.handleExec)
	mux.HandleFunc("POST /destroy", srv.handleDestroy)
	mux.HandleFunc("POST /snapshot/create", srv.handleSnapshotCreate)
	mux.HandleFunc("POST /snapshot/restore", srv.handleSnapshotRestore)
	mux.HandleFunc("GET /snapshot/list", srv.handleSnapshotList)
	mux.HandleFunc("POST /snapshot/delete", srv.handleSnapshotDelete)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	httpServer := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           loggingMiddleware(mux),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Printf("server listening on %s", cfg.ListenAddr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http server error: %v", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Printf("shutdown signal received, cleaning up")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err := httpServer.Shutdown(ctx); err != nil {
		log.Printf("http shutdown error: %v", err)
	}
	if srv.netnsPool != nil {
		srv.netnsPool.Destroy()
	}
	srv.destroyAll()
}
