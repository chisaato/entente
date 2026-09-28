// Command entente is an Envoy control plane that turns Docker container labels
// and static YAML into CDS/EDS/RDS over a Unix domain socket.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	cplog "github.com/envoyproxy/go-control-plane/pkg/log"

	"github.com/chisaato/entente/internal/config"
	"github.com/chisaato/entente/internal/docker"
	"github.com/chisaato/entente/internal/reconcile"
	"github.com/chisaato/entente/internal/xds"
)

func main() {
	configPath := flag.String("config", "", "path to the YAML config file (required)")
	logLevel := flag.String("log-level", "info", "debug, info, warn or error")
	checkOnly := flag.Bool("check", false, "validate the config file and exit")
	flag.Parse()

	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		fmt.Fprintf(os.Stderr, "invalid -log-level: %v\n", err)
		os.Exit(2)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	if *configPath == "" {
		fmt.Fprintln(os.Stderr, "-config is required")
		flag.Usage()
		os.Exit(2)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Error("loading config", "path", *configPath, "err", err)
		os.Exit(1)
	}
	if *checkOnly {
		fmt.Println("config OK")
		return
	}

	if err := run(cfg, *configPath, log); err != nil {
		log.Error("exiting", "err", err)
		os.Exit(1)
	}
}

func run(cfg *config.Config, configPath string, log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// ads=false even though Envoy talks ADS: in "ads" mode the cache holds RDS
	// responses until Envoy names every RouteConfiguration in the snapshot, so
	// one declared entrypoint missing from the bootstrap would stall them all.
	snapshots := cache.NewSnapshotCache(false, cache.IDHash{}, cpLogger(log.With("component", "cache")))
	routeNames := xds.NewRouteNames(nil)
	rec := reconcile.New(cfg, snapshots, routeNames, log.With("component", "reconciler"))

	var wg sync.WaitGroup
	goRun := func(f func()) {
		wg.Add(1)
		go func() { defer wg.Done(); f() }()
	}

	if cfg.Docker != nil {
		w, err := docker.NewWatcher(cfg.Docker.Host, log.With("component", "docker"), rec.SetContainers)
		if err != nil {
			return fmt.Errorf("docker client: %w", err)
		}
		defer w.Close()
		goRun(func() { w.Run(ctx) })
	}

	goRun(func() { rec.Run(ctx) })

	current := cfg
	goRun(func() {
		err := config.Watch(ctx, configPath, log.With("component", "config"), func(next *config.Config) {
			if fields := config.RestartRequired(current, next); len(fields) > 0 {
				log.Error("config reload rejected: these fields need a restart", "fields", fields)
				return
			}
			current = next
			log.Info("config reloaded")
			rec.SetConfig(next)
		})
		if err != nil {
			log.Error("config watcher stopped; hot reload disabled", "err", err)
		}
	})

	srv := xds.NewServer(ctx, snapshots, routeNames, cfg.NodeID, cfg.XDS.Socket, cfg.SocketFileMode(), log.With("component", "xds"))
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve() }()

	var err error
	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case err = <-serveErr:
		stop()
	}
	srv.Stop()
	wg.Wait()
	os.Remove(cfg.XDS.Socket)
	return err
}

// cpLogger adapts slog to go-control-plane's logger interface.
func cpLogger(l *slog.Logger) cplog.Logger {
	return cplog.LoggerFuncs{
		DebugFunc: func(f string, a ...any) { l.Debug(fmt.Sprintf(f, a...)) },
		InfoFunc:  func(f string, a ...any) { l.Debug(fmt.Sprintf(f, a...)) },
		WarnFunc:  func(f string, a ...any) { l.Warn(fmt.Sprintf(f, a...)) },
		ErrorFunc: func(f string, a ...any) { l.Error(fmt.Sprintf(f, a...)) },
	}
}
