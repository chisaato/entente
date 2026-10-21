// Command entente 是 Envoy 的控制平面:把 Docker 容器 label
// 和静态 YAML 变成 Unix domain socket 上的 CDS/EDS/RDS。
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	cplog "github.com/envoyproxy/go-control-plane/pkg/log"
	"github.com/spf13/cobra"

	"github.com/chisaato/entente/internal/config"
	"github.com/chisaato/entente/internal/docker"
	"github.com/chisaato/entente/internal/reconcile"
	"github.com/chisaato/entente/internal/secrets"
	"github.com/chisaato/entente/internal/xds"
)

// version 在发布构建时由 -ldflags "-X main.version=..." 注入。
var version = "dev"

// exitError 携带进程退出码。RunE 返回它时,main 按该码退出,
// 并且不再让 cobra 额外打印一遍错误。
type exitError struct {
	code int
}

func (e exitError) Error() string {
	return fmt.Sprintf("exit code %d", e.code)
}

func main() {
	cmd := newRootCommand()
	// 用法和错误都写到 stderr。
	cmd.SetOut(os.Stderr)
	cmd.SetErr(os.Stderr)
	if err := cmd.Execute(); err != nil {
		var exitErr exitError
		if errors.As(err, &exitErr) {
			os.Exit(exitErr.code)
		}
		os.Exit(2)
	}
}

func newRootCommand() *cobra.Command {
	var (
		configPath string
		logLevel   string
		checkOnly  bool
	)

	cmd := &cobra.Command{
		Use:     "entente",
		Version: version,
		Short:   "Envoy control plane for Docker and static YAML",
		Long:    "Entente is an Envoy control plane that turns Docker container labels and static YAML into CDS/EDS/RDS over a Unix domain socket.",
		// 不注册 completion 子命令,避免多出一个原先没有的入口。
		CompletionOptions: cobra.CompletionOptions{DisableDefaultCmd: true},
		RunE: func(cmd *cobra.Command, args []string) error {
			// 业务错误由下面按原语义打印;解析失败仍走 cobra 默认的错误和用法(退出码 2)。
			cmd.SilenceUsage = true
			cmd.SilenceErrors = true

			var level slog.Level
			if err := level.UnmarshalText([]byte(logLevel)); err != nil {
				fmt.Fprintf(os.Stderr, "invalid --log-level: %v\n", err)
				return exitError{code: 2}
			}
			log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

			if configPath == "" {
				fmt.Fprintln(os.Stderr, "--config is required")
				_ = cmd.Usage()
				return exitError{code: 2}
			}

			cfg, err := config.Load(configPath)
			if err != nil {
				log.Error("loading config", "path", configPath, "err", err)
				return exitError{code: 1}
			}
			if checkOnly {
				fmt.Println("config OK")
				return nil
			}

			if err := run(cfg, configPath, log); err != nil {
				log.Error("exiting", "err", err)
				return exitError{code: 1}
			}
			return nil
		},
	}

	cmd.Flags().StringVarP(&configPath, "config", "c", "", "path to the YAML config file (required)")
	cmd.Flags().StringVarP(&logLevel, "log-level", "l", "info", "debug, info, warn or error")
	cmd.Flags().BoolVar(&checkOnly, "check", false, "validate the config file and exit")
	// 与标准库 flag 一样:遇到第一个非选项就停止,不再把后面的参数当 flag。
	cmd.Flags().SetInterspersed(false)
	cmd.AddCommand(newGenCertCommand())

	return cmd
}

func run(cfg *config.Config, configPath string, log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 即使 Envoy 走的是 ADS,这里也用 ads=false:cache 在 "ads" 模式下会扣住 RDS
	// 响应,直到 Envoy 点名快照里的每一份 RouteConfiguration。于是只要有一个已声明的
	// entrypoint 不在 bootstrap 里,所有入口的路由都会卡住。
	snapshots := cache.NewSnapshotCache(false, cache.IDHash{}, cpLogger(log.With("component", "cache")))
	routeNames := xds.NewRouteNames(nil)
	rec := reconcile.New(cfg, snapshots, routeNames, log.With("component", "reconciler"))

	var wg sync.WaitGroup
	goRun := func(f func()) {
		wg.Add(1)
		go func() { defer wg.Done(); f() }()
	}

	var secretManager *secrets.Manager
	if cfg.OpenBao != nil {
		var err error
		secretManager, err = secrets.New(*cfg.OpenBao, cfg.Secrets, log.With("component", "secrets"), rec.SetSecrets)
		if err != nil {
			return fmt.Errorf("openbao: %w", err)
		}
		goRun(func() { secretManager.Run(ctx) })
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
			if secretManager != nil {
				secretManager.SetSecrets(next.Secrets)
			}
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

// cpLogger 把 slog 适配成 go-control-plane 的 logger 接口。
func cpLogger(l *slog.Logger) cplog.Logger {
	return cplog.LoggerFuncs{
		DebugFunc: func(f string, a ...any) { l.Debug(fmt.Sprintf(f, a...)) },
		InfoFunc:  func(f string, a ...any) { l.Debug(fmt.Sprintf(f, a...)) },
		WarnFunc:  func(f string, a ...any) { l.Warn(fmt.Sprintf(f, a...)) },
		ErrorFunc: func(f string, a ...any) { l.Error(fmt.Sprintf(f, a...)) },
	}
}
