package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"
)

func newGenCertCommand() *cobra.Command {
	var name, commonName, outDir string
	var days int
	cmd := &cobra.Command{
		Use: "gen-cert", Short: "Generate a random self-signed certificate for the SNI fallback chain",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// 限制上界以避免 time.Duration 溢出。
			if days <= 0 || int64(days) > int64((1<<63-1)/(24*time.Hour)) {
				return fmt.Errorf("--days must be positive and at most %d", (1<<63-1)/(24*time.Hour))
			}
			if name == "" {
				return fmt.Errorf("--name must not be empty")
			}
			cert, key, err := generateCertificate(commonName, days)
			if err != nil {
				return err
			}
			if outDir != "" {
				return writeCertificate(outDir, cert, key)
			}
			// 显式使用块标量,使输出可以直接粘贴到 static_resources.secrets。
			literal := func(value []byte) *yaml.Node {
				return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Style: yaml.LiteralStyle, Value: string(value)}
			}
			secret := []any{map[string]any{
				"name": name,
				"tls_certificate": map[string]any{
					"certificate_chain": map[string]any{"inline_string": literal(cert)},
					"private_key":       map[string]any{"inline_string": literal(key)},
				},
			}}
			if _, err := fmt.Fprintln(cmd.OutOrStdout(), "# 由 entente gen-cert 生成的自签兜底证书,不用于真实域名认证。"); err != nil {
				return err
			}
			encoder := yaml.NewEncoder(cmd.OutOrStdout())
			encoder.SetIndent(2)
			defer encoder.Close()
			return encoder.Encode(secret)
		},
	}
	// 根命令将帮助写入 stderr,证书 YAML 则始终默认写入 stdout。
	cmd.SetOut(os.Stdout)
	cmd.Flags().StringVar(&name, "name", "", "secret name (required in every output mode)")
	cmd.Flags().StringVar(&commonName, "common-name", "invalid", "certificate CN and DNS SAN")
	cmd.Flags().IntVar(&days, "days", 3650, "validity in days (must be > 0)")
	cmd.Flags().StringVar(&outDir, "out-dir", "", "write tls.crt (0644) and tls.key (0600), refusing to overwrite; omit to print Envoy secret YAML")
	_ = cmd.MarkFlagRequired("name")
	return cmd
}

func generateCertificate(commonName string, days int) ([]byte, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: commonName},
		DNSNames: []string{commonName}, NotBefore: now.Add(-5 * time.Minute),
		NotAfter: now.Add(time.Duration(days) * 24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), nil
}

func writeCertificate(dir string, cert, key []byte) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	// 先独占创建两个文件;第二个失败时撤销本次创建,不留下半套证书。
	certPath, keyPath := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	cf, err := os.OpenFile(certPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		return err
	}
	defer cf.Close()
	kf, err := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		os.Remove(certPath)
		return err
	}
	defer kf.Close()
	if _, err = cf.Write(cert); err == nil {
		_, err = kf.Write(key)
	}
	if err == nil {
		err = cf.Close()
	}
	if err == nil {
		err = kf.Close()
	}
	if err != nil {
		os.Remove(certPath)
		os.Remove(keyPath)
	}
	return err
}
