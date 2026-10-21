package secrets

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/chisaato/entente/internal/config"
	"github.com/fsnotify/fsnotify"
)

const (
	// 与配置热重载相同:合并 acme.sh 连续写入证书链和私钥的几次事件。
	fileDebounce = 250 * time.Millisecond
	// 目录或文件还不存在、或 watch 建不起来时的重试。不是配置项。
	fileRetryMin = time.Second
	fileRetryMax = 30 * time.Second
	// 监听正常时也定期重读。目录被整个换掉时,inotify 可能盯着旧 inode,不能只等下一次事件。
	fileRescan = 30 * time.Second
)

// watchFiles 启动时立刻读一次,然后监听两个 PEM 的父目录。
// 读文件和系统调用都不持有 Manager 锁。
func (m *Manager) watchFiles(ctx context.Context, s *state) {
	var watcher *fsnotify.Watcher
	defer func() {
		if watcher != nil {
			_ = watcher.Close()
		}
	}()
	var events <-chan fsnotify.Event
	var errs <-chan error
	openWatcher := func() {
		if watcher != nil {
			_ = watcher.Close()
			watcher = nil
			events, errs = nil, nil
		}
		created, err := fsnotify.NewWatcher()
		if err != nil {
			m.log.Error("creating certificate watcher; rereading until it can be created", "secret", s.definition.Name, "err", err)
			return
		}
		watcher = created
		events = created.Events
		errs = created.Errors
	}

	timer := time.NewTimer(0)
	defer timer.Stop()
	var debounce *time.Timer
	var debounceC <-chan time.Time
	defer func() {
		if debounce != nil {
			debounce.Stop()
		}
	}()
	backoff := fileRetryMin

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if watcher == nil {
				openWatcher()
			}
			healthy := m.ensureWatches(watcher, s.definition)
			active, ok := m.readFiles(ctx, s)
			if !active {
				return
			}
			if ok && healthy {
				backoff = fileRetryMin
				timer.Reset(fileRescan)
				continue
			}
			timer.Reset(backoff)
			backoff = min(backoff*2, fileRetryMax)
		case ev, ok := <-events:
			if !ok {
				events, errs = nil, nil
				if watcher != nil {
					_ = watcher.Close()
					watcher = nil
				}
				// 通道关闭后不能干等下一次兜底重读。
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(0)
				continue
			}
			if !fileEventRelevant(ev, s.definition) {
				continue
			}
			if debounce == nil {
				debounce = time.NewTimer(fileDebounce)
				debounceC = debounce.C
			}
		case err, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			if err != nil {
				m.log.Error("certificate watch", "secret", s.definition.Name, "err", err)
			}
		case <-debounceC:
			debounce = nil
			debounceC = nil
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(0)
		}
	}
}

func (m *Manager) ensureWatches(watcher *fsnotify.Watcher, d config.Secret) bool {
	if watcher == nil || d.File == nil {
		return false
	}
	listed := map[string]struct{}{}
	for _, path := range watcher.WatchList() {
		listed[filepath.Clean(path)] = struct{}{}
	}
	ok := true
	for _, dir := range parentDirs(d.File.CertificateChain, d.File.PrivateKey) {
		if _, done := listed[dir]; done {
			continue
		}
		if err := watcher.Add(dir); err != nil {
			m.log.Error("watching certificate directory", "secret", d.Name, "dir", dir, "err", err)
			ok = false
		}
	}
	return ok
}

func (m *Manager) readFiles(ctx context.Context, s *state) (active, ok bool) {
	if ctx.Err() != nil {
		return false, false
	}
	d := s.definition
	if d.File == nil {
		return m.store(s, Certificate{}, fmt.Errorf("certificate files are not configured"), "reading certificate files"), false
	}
	certPEM, err := os.ReadFile(d.File.CertificateChain)
	if err != nil {
		if ctx.Err() != nil {
			return false, false
		}
		return m.store(s, Certificate{}, fmt.Errorf("reading certificate chain: %w", err), "reading certificate files"), false
	}
	keyPEM, err := os.ReadFile(d.File.PrivateKey)
	if err != nil {
		if ctx.Err() != nil {
			return false, false
		}
		return m.store(s, Certificate{}, fmt.Errorf("reading private key file: %w", err), "reading certificate files"), false
	}
	if ctx.Err() != nil {
		return false, false
	}
	parsed, err := parsePair(m.log, d.Name, certPEM, keyPEM)
	if err != nil {
		return m.store(s, Certificate{}, err, "reading certificate files"), false
	}
	return m.store(s, parsed, nil, "reading certificate files"), true
}

func parentDirs(paths ...string) []string {
	dirs := make([]string, 0, 2)
	for _, path := range paths {
		dir := filepath.Clean(filepath.Dir(path))
		if !slices.Contains(dirs, dir) {
			dirs = append(dirs, dir)
		}
	}
	return dirs
}

func fileEventRelevant(ev fsnotify.Event, d config.Secret) bool {
	if d.File == nil {
		return false
	}
	base := filepath.Base(ev.Name)
	if base == filepath.Base(d.File.CertificateChain) || base == filepath.Base(d.File.PrivateKey) {
		return true
	}
	cleaned := filepath.Clean(ev.Name)
	for _, dir := range parentDirs(d.File.CertificateChain, d.File.PrivateKey) {
		if cleaned == dir {
			return true
		}
	}
	return false
}
