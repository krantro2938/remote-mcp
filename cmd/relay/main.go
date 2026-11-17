// Command relay is the public server. Agents dial in over WebSocket at
// /agent; MCP clients (claude.ai custom connectors, Claude Code) use /mcp.
// Configuration comes from environment variables, see deploy/relay.env.example.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/krantro2938/remote-mcp/internal/proto"
	"github.com/krantro2938/remote-mcp/internal/relay"
)

func main() {
	level := slog.LevelInfo
	if os.Getenv("DEBUG") != "" {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	cfg, err := relay.ConfigFromEnv()
	if err != nil {
		log.Error("bad configuration", "err", err)
		os.Exit(2)
	}
	srv, err := relay.New(cfg, log)
	if err != nil {
		log.Error("start", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go srv.RunCleanup(ctx)

	hs := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = hs.Shutdown(sctx)
	}()
	log.Info("relay listening", "addr", cfg.ListenAddr, "public", cfg.PublicURL, "version", proto.Version,
		"machines", len(cfg.AgentTokens), "static_mcp_token", cfg.MCPToken != "", "secret_path", cfg.MCPSecretPath != "")
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("listen", "err", err)
		os.Exit(1)
	}
}
