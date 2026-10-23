// Command ptt-server runs the walkie-talkie backend: REST API, voice relay
// and MCP server in a single process.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/plant-squad/ptt-walkie/internal/api"
	"github.com/plant-squad/ptt-walkie/internal/config"
	"github.com/plant-squad/ptt-walkie/internal/store"
	"github.com/plant-squad/ptt-walkie/internal/voice"
)

func main() {
	cfg := config.Load()
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	st, err := store.New(filepath.Join(cfg.DataDir, "state.json"))
	if err != nil {
		slog.Error("load state", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go st.Run(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})
	apiSrv := &api.Server{Store: st, BaseURL: cfg.BaseURL, NearbyRadiusKm: cfg.NearbyRadiusKm}
	hub := voice.NewHub(st, apiSrv, cfg.MaxTalk)
	apiSrv.Presence = hub
	apiSrv.Register(mux)
	mux.HandleFunc("GET /ws", hub.ServeWS)
	go hub.Run(ctx)

	srv := &http.Server{Addr: cfg.Addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()
	slog.Info("ptt-server listening", "addr", cfg.Addr, "data", cfg.DataDir)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("serve", "err", err)
		os.Exit(1)
	}
}
