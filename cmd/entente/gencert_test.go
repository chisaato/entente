package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

func executeGenCert(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := newRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	for _, cmd := range root.Commands() {
		cmd.SetOut(&out)
	}
	root.SetArgs(append([]string{"gen-cert"}, args...))
	err := root.Execute()
	return out.String(), err
}

func TestGenCertYAML(t *testing.T) {
	for _, custom := range []bool{false, true} {
		args := []string{"--name", "sink: # test"}
		cn, days := "invalid", 3650
		if custom {
			args = append(args, "--common-name", "probe.invalid", "--days", "2")
			cn, days = "probe.invalid", 2
		}
		before := time.Now()
		out, err := executeGenCert(t, args...)
		if err != nil {
			t.Fatal(err)
		}
		var secrets []struct {
			Name string `yaml:"name"`
			TLS  struct {
				Cert struct {
					Inline string `yaml:"inline_string"`
				} `yaml:"certificate_chain"`
				Key struct {
					Inline string `yaml:"inline_string"`
				} `yaml:"private_key"`
			} `yaml:"tls_certificate"`
		}
		if err := yaml.Unmarshal([]byte(out), &secrets); err != nil {
			t.Fatal(err)
		}
		if len(secrets) != 1 || secrets[0].Name != "sink: # test" || !strings.Contains(out, "inline_string: |\n") || !strings.HasPrefix(out, "# ") {
			t.Fatalf("YAML 输出不正确: %s", out)
		}
		pair, err := tls.X509KeyPair([]byte(secrets[0].TLS.Cert.Inline), []byte(secrets[0].TLS.Key.Inline))
		if err != nil {
			t.Fatal(err)
		}
		cert, err := x509.ParseCertificate(pair.Certificate[0])
		if err != nil {
			t.Fatal(err)
		}
		if cert.Subject.CommonName != cn || len(cert.DNSNames) != 1 || cert.DNSNames[0] != cn {
			t.Fatalf("CN/SAN 不正确: %+v", cert)
		}
		if delta := cert.NotBefore.Sub(before.Add(-5 * time.Minute)); delta < -time.Second || delta > time.Second {
			t.Fatalf("NotBefore 不正确: %v", cert.NotBefore)
		}
		if delta := cert.NotAfter.Sub(before.Add(time.Duration(days) * 24 * time.Hour)); delta < -time.Second || delta > time.Second {
			t.Fatalf("NotAfter 不正确: %v", cert.NotAfter)
		}
		key, ok := pair.PrivateKey.(*ecdsa.PrivateKey)
		if !ok || key.Curve.Params().Name != "P-256" || cert.SerialNumber.BitLen() > 128 || cert.SerialNumber.Sign() < 0 {
			t.Fatal("密钥或序列号不正确")
		}
		if cert.KeyUsage != x509.KeyUsageDigitalSignature || len(cert.ExtKeyUsage) != 1 || cert.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth {
			t.Fatal("证书用途不正确")
		}
		if err := cert.CheckSignature(cert.SignatureAlgorithm, cert.RawTBSCertificate, cert.Signature); err != nil {
			t.Fatal(err)
		}
	}
}

func TestGenCertFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sink")
	if _, err := executeGenCert(t, "--name", "sink", "--out-dir", dir); err != nil {
		t.Fatal(err)
	}
	for name, mode := range map[string]os.FileMode{"tls.crt": 0644, "tls.key": 0600} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != mode {
			t.Fatalf("%s 权限: %o", name, info.Mode().Perm())
		}
	}
	if _, err := tls.LoadX509KeyPair(filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")); err != nil {
		t.Fatal(err)
	}
	for _, existing := range []string{"tls.crt", "tls.key"} {
		dir := t.TempDir()
		path := filepath.Join(dir, existing)
		if err := os.WriteFile(path, []byte("保留"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := executeGenCert(t, "--name", "sink", "--out-dir", dir); err == nil {
			t.Fatal("应拒绝覆盖")
		}
		data, err := os.ReadFile(path)
		if err != nil || string(data) != "保留" {
			t.Fatal("已有文件被修改")
		}
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) != 1 {
			t.Fatal("失败后留下了半套证书")
		}
	}
}

func TestGenCertInvalidArgs(t *testing.T) {
	for _, args := range [][]string{nil, {"--name", ""}, {"--name", "sink", "--days", "0"}, {"--name", "sink", "--days", "-1"}, {"--name", "sink", "unexpected"}} {
		if _, err := executeGenCert(t, args...); err == nil {
			t.Fatalf("参数应失败: %v", args)
		}
	}
}
