// Command traffic-light-server is the Traffic Light state server: it
// accepts normalized lifecycle events from hooks and serves the single
// aggregated status that every client renders.
//
// It is the sole authority over state — nothing else ever sets a state
// directly (PRD.md §4). State lives in memory only; a restart resets all
// sessions, which is intentional for a live-state tool.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"trafficlight/internal/api"
	"trafficlight/internal/auth"
	"trafficlight/internal/config"
	"trafficlight/internal/sessions"
)

func main() {
	cfg := config.Default()

	flag.StringVar(&cfg.Addr, "addr", cfg.Addr,
		"listen address; loopback by default, LAN exposure is an explicit opt-in")
	flag.StringVar(&cfg.TokenPath, "token-file", cfg.TokenPath,
		"bearer token file, generated on first run with 0600 permissions")
	flag.DurationVar(&cfg.Sessions.DoneDuration, "done-duration", cfg.Sessions.DoneDuration,
		"how long DONE stays visible before reverting to IDLE")
	flag.DurationVar(&cfg.WaitingTooLong, "waiting-too-long", cfg.WaitingTooLong,
		"how long a session may sit in WAITING before it is flagged as urgent")
	// Exposed as flags because protocol.md §7 states outright that the
	// heuristic is a first pass to be tuned empirically — tuning it
	// should not require a rebuild.
	flag.Float64Var(&cfg.Sessions.StaleFactor, "stale-factor", cfg.Sessions.StaleFactor,
		"multiple of a session's own median event interval before silence counts as stale")
	flag.DurationVar(&cfg.Sessions.StaleFloor, "stale-floor", cfg.Sessions.StaleFloor,
		"minimum silence before any session may be marked UNKNOWN, however chatty it was")
	verbose := flag.Bool("verbose", false, "log accepted-but-notable events at debug level")
	flag.Parse()

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	token, err := auth.LoadOrCreate(cfg.TokenPath)
	if err != nil {
		log.Error("token setup failed", "error", err)
		os.Exit(1)
	}

	mgr := sessions.New(cfg.Sessions)
	srv := &http.Server{
		Addr:    cfg.Addr,
		Handler: api.New(cfg, mgr, token, log).Handler(),
		// Kept short: a hook that somehow hangs must not tie up the
		// server, and the server must never be the slow party.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// The token is deliberately absent from this line and from every
	// other log line and response (PRD.md §8).
	log.Info("traffic light server listening",
		"addr", cfg.Addr, "version", config.Version, "tokenFile", cfg.TokenPath)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		log.Error("server failed", "error", err)
		os.Exit(1)
	case <-ctx.Done():
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Error("shutdown failed", "error", err)
		}
	}
}
