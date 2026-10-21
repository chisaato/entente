package config

import (
	"context"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Watch 在配置文件变化时重新加载,并把每一份能成功加载且通过校验的配置交给 onChange。
// 无效配置只记日志并跳过,继续沿用上一份配置。
//
// 监听的是父目录而不是文件本身,因此编辑器通过 rename 替换文件、
// 以及 Kubernetes 风格的符号链接切换都能被看到。
func Watch(ctx context.Context, path string, log *slog.Logger, onChange func(*Config)) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer w.Close()
	dir, file := filepath.Split(filepath.Clean(path))
	if dir == "" {
		dir = "."
	}
	if err := w.Add(dir); err != nil {
		return err
	}

	var timer <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-w.Errors:
			log.Error("config watch", "err", err)
		case ev := <-w.Events:
			if filepath.Base(ev.Name) != file && !ev.Has(fsnotify.Create) {
				continue
			}
			if timer == nil {
				timer = time.After(250 * time.Millisecond)
			}
		case <-timer:
			timer = nil
			cfg, err := Load(path)
			if err != nil {
				log.Error("config reload failed; keeping previous config", "path", path, "err", err)
				continue
			}
			onChange(cfg)
		}
	}
}
