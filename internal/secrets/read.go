package secrets

import (
	"context"
	"fmt"
	"os"
)

import "github.com/chisaato/entente/internal/config"

// Read 为候选读取一次材料,不改动生效任务或最后一份有效证书。
func (m *Manager) Read(ctx context.Context, d config.Secret) (Certificate, error) {
	if d.File == nil {
		return m.fetch(ctx, d)
	}
	cert, err := os.ReadFile(d.File.CertificateChain)
	if err != nil {
		return Certificate{}, fmt.Errorf("reading certificate chain: %w", err)
	}
	key, err := os.ReadFile(d.File.PrivateKey)
	if err != nil {
		return Certificate{}, fmt.Errorf("reading private key: %w", err)
	}
	return parsePair(m.log, d.Name, cert, key)
}

// SetValidator 在保存续期结果前检查生效绑定,失败时由 store 保留旧材料。
// 校验函数不得回调 Manager。
func (m *Manager) SetValidator(validate func(Certificate) error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.validate = validate
}

// Commit 使用已经校验过的同一份材料切换任务,避免提交时重复读取。
func (m *Manager) Commit(definitions []config.Secret, certificates []Certificate) {
	m.setSecrets(definitions, certificates)
}
