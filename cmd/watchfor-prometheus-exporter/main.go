// Command watchfor-prometheus-exporter polls the WatchFor Prometheus endpoint
// (GET /api/v1/metrics) for one or more organizations and serves the merged
// result, with an organization label on every series, on /metrics.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/watchfor-io/prometheus-exporter/internal/config"
	"github.com/watchfor-io/prometheus-exporter/internal/exporter"
)

// Version is set at build time: -ldflags "-X main.Version=0.1.0".
var Version = "dev"

// Revision overrides the commit Go stamps from the checkout; set it with
// -ldflags "-X main.Revision=<sha>" where no .git is available (the
// container build).
var Revision = ""

func main() {
	os.Exit(run(os.Args[1:], os.LookupEnv, os.Stdout, os.Stderr))
}

func run(args []string, env config.Env, stdout, stderr io.Writer) int {
	cfg, opts, err := config.Load(args, env, stderr)
	switch {
	case errors.Is(err, config.ErrHelp):
		return 0
	case err != nil:
		fmt.Fprintf(stderr, "watchfor-prometheus-exporter: invalid configuration:\n%s\n", err)
		return 2
	case opts.ShowVersion:
		fmt.Fprintf(stdout, "watchfor-prometheus-exporter %s (revision %s, %s)\n", Version, revision(), runtime.Version())
		return 0
	}

	log := newLogger(stderr, cfg.Log)
	orgs := make([]string, 0, len(cfg.Targets))
	for _, t := range cfg.Targets {
		orgs = append(orgs, t.Organization)
	}
	log.Info("starting", "version", Version, "revision", revision(), "config", cfg.Source,
		"listen", cfg.ListenAddress, "interval", time.Duration(cfg.Interval).String(), "organizations", orgs)

	exp := exporter.New(exporter.Options{
		Targets:   cfg.Targets,
		Interval:  time.Duration(cfg.Interval),
		Timeout:   time.Duration(cfg.Timeout),
		Version:   Version,
		Revision:  revision(),
		GoVersion: runtime.Version(),
		Logger:    log,
	})
	exp.Registry().MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	ln, err := net.Listen("tcp", cfg.ListenAddress)
	if err != nil {
		log.Error("cannot listen", "address", cfg.ListenAddress, "error", err)
		return 1
	}
	srv := &http.Server{
		Handler:           exporter.Handler(exp, Version, log),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		exp.Run(ctx)
	}()

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	log.Info("serving", "address", ln.Addr().String())

	code := 0
	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case err := <-serveErr:
		log.Error("server stopped", "error", err)
		code = 1
		stop()
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Warn("shutdown", "error", err)
	}
	wg.Wait()
	return code
}

func newLogger(w io.Writer, c config.Log) *slog.Logger {
	var level slog.Level
	_ = level.UnmarshalText([]byte(c.Level))
	opts := &slog.HandlerOptions{Level: level}
	if c.Format == "json" {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}

// revision is the commit the binary was built from, as Go stamps it.
func revision() string {
	if Revision != "" {
		return shortRev(Revision)
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	rev, dirty := "", false
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return "unknown"
	}
	rev = shortRev(rev)
	if dirty {
		rev += "-dirty"
	}
	return rev
}

func shortRev(rev string) string {
	if len(rev) > 12 {
		return rev[:12]
	}
	return rev
}
