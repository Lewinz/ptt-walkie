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

	"github.com/plant-squad/ptt-walkie/internal/ai"
	"github.com/plant-squad/ptt-walkie/internal/api"
	"github.com/plant-squad/ptt-walkie/internal/archive"
	"github.com/plant-squad/ptt-walkie/internal/assistant"
	"github.com/plant-squad/ptt-walkie/internal/config"
	"github.com/plant-squad/ptt-walkie/internal/kodo"
	"github.com/plant-squad/ptt-walkie/internal/lifecycle"
	"github.com/plant-squad/ptt-walkie/internal/mcp"
	"github.com/plant-squad/ptt-walkie/internal/social"
	"github.com/plant-squad/ptt-walkie/internal/stats"
	"github.com/plant-squad/ptt-walkie/internal/store"
	"github.com/plant-squad/ptt-walkie/internal/voice"
	"github.com/plant-squad/ptt-walkie/web"
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
	apiSrv := &api.Server{Store: st, BaseURL: cfg.BaseURL, NearbyRadiusKm: cfg.NearbyRadiusKm, AdminToken: cfg.AuthToken}
	hub := voice.NewHub(st, apiSrv, cfg.MaxTalk)
	apiSrv.Presence = hub
	apiSrv.Register(mux)
	mux.HandleFunc("GET /ws", hub.ServeWS)

	arc, err := archive.Open(filepath.Join(cfg.DataDir, "recordings"))
	if err != nil {
		slog.Error("open archive", "err", err)
		os.Exit(1)
	}
	defer arc.Close()
	hub.AddRecorder(arc)
	arcHandler := &archive.Handler{Archive: arc, Store: st, Hub: hub}
	arcHandler.Register(mux)
	stats.Register(mux, arc, st)

	aiSvc := &ai.Service{Archive: arc, Replay: arcHandler, Store: st, Hub: hub,
		PublicURL: cfg.BaseURL, AlertWords: cfg.AlertWords}
	if cfg.AIKey != "" {
		aiSvc.Client = &ai.Client{BaseURL: cfg.AIBaseURL, Key: cfg.AIKey, Model: cfg.AIModel,
			ASRMode: cfg.ASRMode, ASRModel: cfg.ASRModel}
		slog.Info("ai enabled", "base", cfg.AIBaseURL, "model", cfg.AIModel, "asr", cfg.ASRMode)
	} else {
		slog.Info("ai disabled (set PTT_AI_KEY); accepting client-side transcripts")
	}
	if cfg.KodoEnabled() {
		up := &kodo.Uploader{Archive: arc, Prefix: cfg.KodoPrefix, Client: &kodo.Client{
			Endpoint: cfg.KodoEndpoint, Region: cfg.KodoRegionOrDefault(), Bucket: cfg.KodoBucket,
			AccessKey: cfg.KodoAK, SecretKey: cfg.KodoSK}}
		up.Attach()
		aiSvc.AudioURL = up.URL
		arcHandler.RemoteURL = up.URL
		slog.Info("kodo mirror enabled", "endpoint", cfg.KodoEndpoint, "bucket", cfg.KodoBucket)
	}
	aiSvc.Attach()
	aiSvc.Register(mux)

	soc := &social.Service{Store: st, Hub: hub}
	hub.OnPair = soc.HandlePairButton
	soc.Register(mux)

	mcpSrv := mcp.NewServer(mcp.Deps{Store: st, API: apiSrv, Hub: hub, Archive: arcHandler, AI: aiSvc, Social: soc,
		NearbyRadiusKm: cfg.NearbyRadiusKm})
	(&mcp.HTTPHandler{Server: mcpSrv, Token: cfg.AuthToken}).Register(mux)
	(&assistant.Assistant{MCP: mcpSrv, LLM: aiSvc.Client}).Register(mux)
	web.Register(mux)
	for _, ep := range mcp.ParseEndpoints(cfg.MCPEndpoint) {
		go mcpSrv.RunBridge(ctx, ep)
	}
	if cfg.Demo {
		go runEchoBot(ctx, localURL(cfg), seedDemo(st))
	}
	go hub.Run(ctx)
	go (&lifecycle.Janitor{Store: st, Hub: hub, DefaultIdle: cfg.TempGroupIdle}).Run(ctx, time.Minute)

	srv := &http.Server{Addr: cfg.Addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()
	slog.Info("ptt-server listening", "addr", cfg.Addr, "data", cfg.DataDir, "tls", cfg.TLSCert != "",
		"device", cfg.BaseURL+"/device", "admin", cfg.BaseURL+"/admin", "mcp", cfg.BaseURL+"/mcp/d/{device_id}")
	if cfg.TLSCert != "" {
		err = srv.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey)
	} else {
		err = srv.ListenAndServe()
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("serve", "err", err)
		os.Exit(1)
	}
	if err := st.Flush(); err != nil {
		slog.Error("flush state", "err", err)
	}
}
