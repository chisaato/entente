// Package secrets 从 OpenBao KV v2 或本地 PEM 文件读取证书,并在内存中保留最后一份有效内容。
// 私钥只存在于内存中的 Certificate 里,任何日志都不得输出证书或私钥字节。
package secrets

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/chisaato/entente/internal/config"
)

const requestTimeout = 10 * time.Second

// Certificate 只在内存中传递,不得写入日志。
type Certificate struct {
	Name        string
	Certificate []byte
	PrivateKey  []byte
}

type state struct {
	definition  config.Secret
	certificate *Certificate
	attempted   bool
	cancel      context.CancelFunc
}

type Manager struct {
	mu       sync.Mutex
	cfg      *config.OpenBao
	client   *http.Client
	interval time.Duration
	log      *slog.Logger
	onChange func([]Certificate, bool)
	states   map[string]*state
	ctx      context.Context
	wg       sync.WaitGroup
}

// New 创建读取器。cfg 为 nil 时只服务本地文件来源,不建立 HTTP 客户端。
// onChange 的第二个参数表示当前列表均已完成首次尝试。
func New(cfg *config.OpenBao, definitions []config.Secret, log *slog.Logger, onChange func([]Certificate, bool)) (*Manager, error) {
	m := &Manager{log: log, onChange: onChange, states: map[string]*state{}}
	if cfg != nil {
		interval, err := time.ParseDuration(cfg.RefreshInterval)
		if err != nil || interval <= 0 {
			return nil, fmt.Errorf("invalid openbao refresh interval")
		}
		u, err := url.Parse(cfg.Address)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return nil, fmt.Errorf("invalid openbao address")
		}
		transport := http.DefaultTransport.(*http.Transport).Clone()
		if cfg.CAFile != "" {
			pem, err := os.ReadFile(cfg.CAFile)
			if err != nil {
				return nil, fmt.Errorf("reading openbao CA: %w", err)
			}
			roots, err := x509.SystemCertPool()
			if err != nil {
				roots = x509.NewCertPool()
			}
			if !roots.AppendCertsFromPEM(pem) {
				return nil, fmt.Errorf("openbao CA file contains no certificates")
			}
			transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
		}
		copied := *cfg
		m.cfg = &copied
		m.interval = interval
		m.client = &http.Client{Transport: transport, Timeout: requestTimeout,
			// 不跟随重定向,避免把 token 转发到其他地址。
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	m.SetSecrets(definitions)
	return m, nil
}

// SetSecrets 保留未变化的任务,删除立即生效,新增或修改的任务立即读取。
func (m *Manager) SetSecrets(definitions []config.Secret) {
	m.mu.Lock()
	defer m.mu.Unlock()
	next := make(map[string]*state, len(definitions))
	for _, d := range definitions {
		s := m.states[d.Name]
		if s == nil || !s.definition.Equal(d) {
			// 同名配置迁移读取位置时,失败仍保留上一份有效内容。
			var previous *Certificate
			if s != nil {
				previous = s.certificate
			}
			if s != nil && s.cancel != nil {
				s.cancel()
			}
			s = &state{definition: d, certificate: previous}
			if m.ctx != nil {
				m.start(s)
			}
		}
		next[d.Name] = s
	}
	for name, s := range m.states {
		if next[name] == nil && s.cancel != nil {
			s.cancel()
		}
	}
	m.states = next
	m.notify()
}

// Run 启动独立轮询,取消后等待全部请求结束。
func (m *Manager) Run(ctx context.Context) {
	m.mu.Lock()
	m.ctx = ctx
	for _, s := range m.states {
		m.start(s)
	}
	m.mu.Unlock()
	<-ctx.Done()
	m.mu.Lock()
	// 禁止退出过程中继续新增 goroutine。
	m.ctx = nil
	m.mu.Unlock()
	m.wg.Wait()
	if m.client != nil {
		m.client.CloseIdleConnections()
	}
}

func (m *Manager) start(s *state) {
	ctx, cancel := context.WithCancel(m.ctx)
	s.cancel = cancel
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		if s.definition.File != nil {
			m.watchFiles(ctx, s)
			return
		}
		m.poll(ctx, s)
	}()
}

// notify 在锁内串行回调,保证旧任务无法覆盖新列表的状态。
func (m *Manager) notify() {
	certificates := []Certificate{}
	synced := true
	for _, s := range m.states {
		synced = synced && s.attempted
		if s.certificate != nil {
			c := *s.certificate
			c.Certificate = slices.Clone(c.Certificate)
			c.PrivateKey = slices.Clone(c.PrivateKey)
			certificates = append(certificates, c)
		}
	}
	slices.SortFunc(certificates, func(a, b Certificate) int { return strings.Compare(a.Name, b.Name) })
	if m.onChange != nil {
		m.onChange(certificates, synced)
	}
}

func (m *Manager) poll(ctx context.Context, s *state) {
	if m.interval <= 0 {
		m.store(s, Certificate{}, fmt.Errorf("openbao is not configured"), "fetching secret")
		<-ctx.Done()
		return
	}
	backoff := min(time.Second, m.interval)
	for {
		c, err := m.fetch(ctx, s.definition)
		if ctx.Err() != nil {
			return
		}
		if !m.store(s, c, err, "fetching secret") {
			return
		}
		delay := m.interval
		if err != nil {
			delay = backoff
			backoff += min(backoff, m.interval-backoff)
		} else {
			backoff = min(time.Second, m.interval)
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (m *Manager) fetch(ctx context.Context, d config.Secret) (Certificate, error) {
	var empty Certificate
	if m.cfg == nil || m.client == nil {
		return empty, fmt.Errorf("openbao is not configured")
	}
	token, err := os.ReadFile(m.cfg.TokenFile)
	if err != nil {
		return empty, fmt.Errorf("reading openbao token file: %w", err)
	}
	if strings.TrimSpace(string(token)) == "" {
		return empty, fmt.Errorf("openbao token file is empty")
	}
	address := strings.TrimRight(m.cfg.Address, "/") + "/v1/" + strings.Trim(d.KVMount, "/") + "/data/" + strings.TrimLeft(d.KVPath, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return empty, fmt.Errorf("constructing openbao request")
	}
	req.Header.Set("X-Vault-Token", strings.TrimSpace(string(token)))
	resp, err := m.client.Do(req)
	// 底层错误可能包含敏感请求信息,只报告固定描述。
	if err != nil {
		return empty, fmt.Errorf("openbao request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return empty, fmt.Errorf("openbao returned HTTP %d", resp.StatusCode)
	}
	var payload struct {
		Data struct {
			Data map[string]any `json:"data"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return empty, fmt.Errorf("invalid openbao KV v2 response")
	}
	cert, okCert := payload.Data.Data[d.CertificateField].(string)
	key, okKey := payload.Data.Data[d.PrivateKeyField].(string)
	if !okCert || !okKey || strings.TrimSpace(cert) == "" || strings.TrimSpace(key) == "" {
		return empty, fmt.Errorf("certificate fields must be non-empty strings")
	}
	return parsePair(m.log, d.Name, []byte(cert), []byte(key))
}

// store 在确认 s 仍是当前任务后更新 last-good。返回 false 表示任务已被替换,调用方应退出。
// 证书内容不变且已经尝试过时不回调,避免相同字节再发一份快照。
func (m *Manager) store(s *state, c Certificate, err error, action string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.states[s.definition.Name] != s {
		return false
	}
	changed := !s.attempted
	s.attempted = true
	if err != nil {
		// 从未成功时没有"上一份"可保留,日志不能暗示证书已在下发。
		if s.certificate == nil {
			m.log.Error(action+"; no valid certificate yet, not served", "secret", s.definition.Name, "err", err)
		} else {
			m.log.Error(action+"; keeping last valid certificate", "secret", s.definition.Name, "err", err)
		}
	} else {
		changed = changed || s.certificate == nil || !slices.Equal(c.Certificate, s.certificate.Certificate) || !slices.Equal(c.PrivateKey, s.certificate.PrivateKey)
		if changed {
			copied := c
			s.certificate = &copied
		}
	}
	if changed {
		m.notify()
	}
	return true
}

// parsePair 校验 PEM 证书链与私钥成对、可解析。失败信息是固定文本,不包含文件内容。
func parsePair(log *slog.Logger, name string, certPEM, keyPEM []byte) (Certificate, error) {
	var empty Certificate
	if len(bytes.TrimSpace(certPEM)) == 0 || len(bytes.TrimSpace(keyPEM)) == 0 {
		return empty, fmt.Errorf("certificate and private key must be non-empty")
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return empty, fmt.Errorf("invalid TLS certificate or mismatched private key")
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return empty, fmt.Errorf("invalid leaf certificate")
	}
	if time.Until(leaf.NotAfter) <= 7*24*time.Hour {
		log.Warn("TLS certificate expired or expires within seven days", "secret", name, "not_after", leaf.NotAfter)
	}
	return Certificate{Name: name, Certificate: bytes.Clone(certPEM), PrivateKey: bytes.Clone(keyPEM)}, nil
}
