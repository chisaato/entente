package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"strings"
	"time"

	"github.com/chisaato/entente/internal/config"
	"github.com/chisaato/entente/internal/docker"
	"github.com/chisaato/entente/internal/reconcile"
	"github.com/chisaato/entente/internal/secrets"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// checkConfig 与运行中的候选共用 Prepare;离线模式绝不创建外部客户端。
func checkConfig(ctx context.Context, cfg *config.Config, online bool, out io.Writer, log *slog.Logger) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var bao *config.OpenBao
	if online {
		bao = cfg.OpenBao
	}
	reader, err := secrets.New(bao, nil, log, nil)
	if err != nil {
		return err
	}
	materials := make(map[string]reconcile.Material, len(cfg.Secrets))
	for _, definition := range cfg.Secrets {
		if definition.File == nil && !online {
			materials[definition.Name] = reconcile.Material{Unchecked: true}
			continue
		}
		certificate, readErr := reader.Read(ctx, definition)
		materials[definition.Name] = reconcile.Material{Certificate: certificate, Err: readErr}
	}
	var containers []docker.Container
	if online && cfg.Docker != nil {
		containers, err = checkDockerSnapshot(ctx, cfg.Docker.Host)
		if err != nil {
			fmt.Fprintln(out, "未检查: Docker 快照(读取失败)")
			return fmt.Errorf("Docker snapshot: %w", err)
		}
	}
	prepared, err := reconcile.Prepare(cfg, containers, materials, reconcile.PrepareOptions{Now: time.Now(), Offline: !online})
	if err == nil {
		fmt.Fprintln(out, "已检查: 配置文件、静态路由、交叉引用、xDS 资源")
	} else {
		fmt.Fprintln(out, "检查失败: 候选配置无效")
	}
	if online && cfg.Docker != nil {
		fmt.Fprintln(out, "已检查: Docker 快照")
	}
	for _, item := range prepared.Report.Checked {
		fmt.Fprintln(out, "已检查:", item)
	}
	for _, item := range prepared.Report.Unchecked {
		fmt.Fprintln(out, "未检查:", item)
	}
	for _, warning := range prepared.Report.Warnings {
		fmt.Fprintln(out, "警告:", warning)
	}
	if len(prepared.Report.Unchecked) == 0 {
		fmt.Fprintln(out, "未检查: 无")
	}
	if len(prepared.Report.Warnings) == 0 {
		fmt.Fprintln(out, "警告: 无")
	}
	return err
}

// checkDockerSnapshot 只做一次 list,不启动 watcher 或事件订阅。
func checkDockerSnapshot(ctx context.Context, host string) ([]docker.Container, error) {
	cli, err := client.New(client.WithHost(host))
	if err != nil {
		return nil, err
	}
	defer cli.Close()
	result, err := cli.ContainerList(ctx, client.ContainerListOptions{})
	if err != nil {
		return nil, err
	}
	snapshot := make([]docker.Container, 0, len(result.Items))
	for _, item := range result.Items {
		if item.State != container.StateRunning {
			continue
		}
		c := docker.Container{ID: item.ID, Labels: item.Labels, Networks: map[string]netip.Addr{}}
		if len(item.Names) > 0 {
			c.Name = strings.TrimPrefix(item.Names[0], "/")
		}
		if item.NetworkSettings != nil {
			for name, endpoint := range item.NetworkSettings.Networks {
				if endpoint == nil {
					continue
				}
				address := endpoint.IPAddress
				if !address.IsValid() {
					address = endpoint.GlobalIPv6Address
				}
				c.Networks[name] = address
			}
		}
		snapshot = append(snapshot, c)
	}
	return snapshot, nil
}
