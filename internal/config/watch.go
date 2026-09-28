package config

import (
	"context"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Watch reloads the config file whenever it changes and passes every config
// that loads and validates to onChange. Invalid configs are logged and
// skipped; the previous config stays in effect.
//
// The parent directory is watched rather than the file, so editors that
// replace the file via rename and Kubernetes-style symlink swaps are seen.
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
