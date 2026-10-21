package secrets

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chisaato/entente/internal/config"
)

func testCertificate(t *testing.T) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "example.test"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(30 * 24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	encodedKey, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encodedKey}))
}

func testDefinition(name string) config.Secret {
	return config.Secret{Name: name, KVMount: "secret", KVPath: "entente/demo", CertificateField: "fullchain", PrivateKeyField: "key"}
}

func newTestManager(t *testing.T, server *httptest.Server, callback func([]Certificate, bool)) (*Manager, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(" first-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := New(&config.OpenBao{Address: server.URL, TokenFile: path, RefreshInterval: "20ms"}, []config.Secret{testDefinition("cert")}, slog.New(slog.NewTextHandler(io.Discard, nil)), callback)
	if err != nil {
		t.Fatal(err)
	}
	return m, path
}

func writeKV(w http.ResponseWriter, cert, key any) {
	_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"data": map[string]any{"fullchain": cert, "key": key}}})
}

func TestFetchValidationAndTokenReload(t *testing.T) {
	cert, key := testCertificate(t)
	_, otherKey := testCertificate(t)
	var responseCert, responseKey any = cert, key
	wantToken, status := "first-token", http.StatusOK
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/secret/data/entente/demo" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.Header.Get("X-Vault-Token") != wantToken {
			t.Errorf("token not reloaded")
		}
		w.WriteHeader(status)
		writeKV(w, responseCert, responseKey)
	}))
	defer server.Close()
	m, path := newTestManager(t, server, nil)
	defer m.client.CloseIdleConnections()
	got, err := m.fetch(context.Background(), testDefinition("cert"))
	if err != nil || string(got.Certificate) != cert || string(got.PrivateKey) != key {
		t.Fatalf("successful fetch: err=%v", err)
	}
	wantToken = "second-token"
	if err := os.WriteFile(path, []byte("second-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.fetch(context.Background(), testDefinition("cert")); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name      string
		cert, key any
		status    int
	}{
		{"bad PEM", "broken", key, 200},
		{"mismatched key", cert, otherKey, 200},
		{"empty certificate", "", key, 200},
		{"non-string key", cert, 42, 200},
		{"forbidden", cert, key, 403},
		{"server error", cert, key, 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			responseCert, responseKey, status = tc.cert, tc.key, tc.status
			if _, err := m.fetch(context.Background(), testDefinition("cert")); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

type update struct {
	certificates []Certificate
	synced       bool
}

func awaitUpdate(t *testing.T, updates <-chan update, want func(update) bool) update {
	t.Helper()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		select {
		case u := <-updates:
			if want(u) {
				return u
			}
		case <-timer.C:
			t.Fatal("timed out waiting for secret update")
			return update{}
		}
	}
}

func TestPollingRetainsLastValidAndDeletes(t *testing.T) {
	cert, key := testCertificate(t)
	var mu sync.Mutex
	status, calls := 200, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		code := status
		calls++
		mu.Unlock()
		w.WriteHeader(code)
		writeKV(w, cert, key)
	}))
	defer server.Close()
	updates := make(chan update, 100)
	m, _ := newTestManager(t, server, func(cs []Certificate, synced bool) { updates <- update{cs, synced} })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	awaitUpdate(t, updates, func(u update) bool { return u.synced && len(u.certificates) == 1 })
	for _, code := range []int{403, 500} {
		mu.Lock()
		status = code
		before := calls
		mu.Unlock()
		deadline := time.Now().Add(time.Second)
		for {
			mu.Lock()
			count := calls
			mu.Unlock()
			if count > before {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("poll did not retry")
			}
			time.Sleep(time.Millisecond)
		}
		m.mu.Lock()
		if m.states["cert"].certificate == nil || !slices.Equal(m.states["cert"].certificate.Certificate, []byte(cert)) {
			t.Error("failed refresh discarded last certificate")
		}
		m.mu.Unlock()
	}
	m.SetSecrets(nil)
	awaitUpdate(t, updates, func(u update) bool { return u.synced && len(u.certificates) == 0 })
	// 删除后任务不再轮询。
	time.Sleep(30 * time.Millisecond)
	mu.Lock()
	before := calls
	mu.Unlock()
	time.Sleep(60 * time.Millisecond)
	mu.Lock()
	after := calls
	mu.Unlock()
	if before != after {
		t.Error("deleted secret still polled")
	}
}

func TestFirstFailureAndHotAdd(t *testing.T) {
	cert, key := testCertificate(t)
	var mu sync.Mutex
	success := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ok := success
		mu.Unlock()
		if !ok {
			w.WriteHeader(403)
			return
		}
		writeKV(w, cert, key)
	}))
	defer server.Close()
	updates := make(chan update, 100)
	m, _ := newTestManager(t, server, func(cs []Certificate, synced bool) { updates <- update{cs, synced} })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	awaitUpdate(t, updates, func(u update) bool { return u.synced && len(u.certificates) == 0 })
	mu.Lock()
	success = true
	mu.Unlock()
	awaitUpdate(t, updates, func(u update) bool { return u.synced && len(u.certificates) == 1 })
	m.SetSecrets([]config.Secret{testDefinition("cert"), testDefinition("added")})
	awaitUpdate(t, updates, func(u update) bool { return !u.synced })
	u := awaitUpdate(t, updates, func(u update) bool { return u.synced && len(u.certificates) == 2 })
	if u.certificates[0].Name != "added" {
		t.Error("certificates not sorted")
	}
}

func fileSecret(name, chain, key string) config.Secret {
	return config.Secret{Name: name, File: &config.SecretFile{CertificateChain: chain, PrivateKey: key}}
}

func writePEM(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func startManager(t *testing.T, m *Manager) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("manager did not stop; watcher leaked")
		}
	})
}

func TestFileAppearsAfterMissingDirectory(t *testing.T) {
	cert, key := testCertificate(t)
	dir := filepath.Join(t.TempDir(), "later")
	chain, keyPath := filepath.Join(dir, "fullchain.pem"), filepath.Join(dir, "key.pem")
	updates := make(chan update, 100)
	m, err := New(nil, []config.Secret{fileSecret("cert", chain, keyPath)}, slog.New(slog.NewTextHandler(io.Discard, nil)), func(cs []Certificate, synced bool) {
		updates <- update{cs, synced}
	})
	if err != nil {
		t.Fatal(err)
	}
	if m.client != nil {
		t.Fatal("file source created an HTTP client")
	}
	startManager(t, m)
	awaitUpdate(t, updates, func(u update) bool { return u.synced && len(u.certificates) == 0 })
	writePEM(t, chain, cert)
	writePEM(t, keyPath, key)
	u := awaitUpdate(t, updates, func(u update) bool { return u.synced && len(u.certificates) == 1 })
	if string(u.certificates[0].Certificate) != cert || string(u.certificates[0].PrivateKey) != key {
		t.Fatal("recovered certificate mismatch")
	}
}

func TestFileInPlaceAndRename(t *testing.T) {
	cert, key := testCertificate(t)
	secondCert, secondKey := testCertificate(t)
	thirdCert, thirdKey := testCertificate(t)
	root := t.TempDir()
	chain, keyPath := filepath.Join(root, "certs", "fullchain.pem"), filepath.Join(root, "keys", "key.pem")
	writePEM(t, chain, cert)
	writePEM(t, keyPath, key)
	updates := make(chan update, 100)
	m, err := New(nil, []config.Secret{fileSecret("cert", chain, keyPath)}, slog.New(slog.NewTextHandler(io.Discard, nil)), func(cs []Certificate, synced bool) {
		updates <- update{cs, synced}
	})
	if err != nil {
		t.Fatal(err)
	}
	startManager(t, m)
	awaitUpdate(t, updates, func(u update) bool {
		return u.synced && len(u.certificates) == 1 && string(u.certificates[0].Certificate) == cert
	})
	writePEM(t, chain, secondCert)
	writePEM(t, keyPath, secondKey)
	awaitUpdate(t, updates, func(u update) bool {
		return u.synced && len(u.certificates) == 1 && string(u.certificates[0].Certificate) == secondCert && string(u.certificates[0].PrivateKey) == secondKey
	})
	writePEM(t, chain+".tmp", thirdCert)
	writePEM(t, keyPath+".tmp", thirdKey)
	if err := os.Rename(chain+".tmp", chain); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(keyPath+".tmp", keyPath); err != nil {
		t.Fatal(err)
	}
	awaitUpdate(t, updates, func(u update) bool {
		return u.synced && len(u.certificates) == 1 && string(u.certificates[0].Certificate) == thirdCert && string(u.certificates[0].PrivateKey) == thirdKey
	})
}

func TestFileMismatchKeepsLastGood(t *testing.T) {
	cert, key := testCertificate(t)
	otherCert, otherKey := testCertificate(t)
	dir := t.TempDir()
	chain, keyPath := filepath.Join(dir, "fullchain.pem"), filepath.Join(dir, "key.pem")
	writePEM(t, chain, cert)
	writePEM(t, keyPath, key)
	updates := make(chan update, 100)
	m, err := New(nil, []config.Secret{fileSecret("cert", chain, keyPath)}, slog.New(slog.NewTextHandler(io.Discard, nil)), func(cs []Certificate, synced bool) {
		updates <- update{cs, synced}
	})
	if err != nil {
		t.Fatal(err)
	}
	startManager(t, m)
	awaitUpdate(t, updates, func(u update) bool { return u.synced && len(u.certificates) == 1 })
	writePEM(t, chain, otherCert)
	select {
	case u := <-updates:
		t.Fatalf("mismatched certificate published: %d certs", len(u.certificates))
	case <-time.After(time.Second):
	}
	m.mu.Lock()
	kept := m.states["cert"].certificate
	m.mu.Unlock()
	if kept == nil || string(kept.Certificate) != cert || string(kept.PrivateKey) != key {
		t.Fatal("mismatched refresh discarded last certificate")
	}
	writePEM(t, keyPath, otherKey)
	u := awaitUpdate(t, updates, func(u update) bool {
		return len(u.certificates) == 1 && string(u.certificates[0].Certificate) == otherCert
	})
	if string(u.certificates[0].PrivateKey) != otherKey {
		t.Fatal("matching key was not published with the new certificate")
	}
}

func TestFileUnchangedContentDoesNotNotify(t *testing.T) {
	cert, key := testCertificate(t)
	dir := t.TempDir()
	chain, keyPath := filepath.Join(dir, "fullchain.pem"), filepath.Join(dir, "key.pem")
	writePEM(t, chain, cert)
	writePEM(t, keyPath, key)
	var n atomic.Int32
	updates := make(chan update, 100)
	m, err := New(nil, []config.Secret{fileSecret("cert", chain, keyPath)}, slog.New(slog.NewTextHandler(io.Discard, nil)), func(cs []Certificate, synced bool) {
		n.Add(1)
		updates <- update{cs, synced}
	})
	if err != nil {
		t.Fatal(err)
	}
	startManager(t, m)
	awaitUpdate(t, updates, func(u update) bool { return u.synced && len(u.certificates) == 1 })
	before := n.Load()
	writePEM(t, chain, cert)
	writePEM(t, keyPath, key)
	time.Sleep(time.Second)
	if n.Load() != before {
		t.Fatalf("identical certificate notified again: before=%d after=%d", before, n.Load())
	}
}

func TestFileReloadPathDeleteAndKeepState(t *testing.T) {
	cert, key := testCertificate(t)
	otherCert, otherKey := testCertificate(t)
	dir1, dir2 := t.TempDir(), t.TempDir()
	chain1, key1 := filepath.Join(dir1, "fullchain.pem"), filepath.Join(dir1, "key.pem")
	chain2, key2 := filepath.Join(dir2, "fullchain.pem"), filepath.Join(dir2, "key.pem")
	writePEM(t, chain1, cert)
	writePEM(t, key1, key)
	writePEM(t, chain2, otherCert)
	writePEM(t, key2, otherKey)
	updates := make(chan update, 100)
	m, err := New(nil, []config.Secret{fileSecret("cert", chain1, key1)}, slog.New(slog.NewTextHandler(io.Discard, nil)), func(cs []Certificate, synced bool) {
		updates <- update{cs, synced}
	})
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	original := m.states["cert"]
	m.mu.Unlock()
	m.SetSecrets([]config.Secret{fileSecret("cert", chain1, key1)})
	m.mu.Lock()
	kept := m.states["cert"]
	m.mu.Unlock()
	if original != kept {
		t.Fatal("reparsed file secret restarted its watcher")
	}
	startManager(t, m)
	awaitUpdate(t, updates, func(u update) bool {
		return u.synced && len(u.certificates) == 1 && string(u.certificates[0].Certificate) == cert
	})
	m.SetSecrets([]config.Secret{fileSecret("cert", chain2, key2)})
	awaitUpdate(t, updates, func(u update) bool {
		return u.synced && len(u.certificates) == 1 && string(u.certificates[0].Certificate) == otherCert
	})
	missing := filepath.Join(t.TempDir(), "missing", "fullchain.pem")
	m.SetSecrets([]config.Secret{fileSecret("cert", missing, filepath.Join(filepath.Dir(missing), "key.pem"))})
	awaitUpdate(t, updates, func(u update) bool {
		return u.synced && len(u.certificates) == 1 && string(u.certificates[0].Certificate) == otherCert
	})
	m.SetSecrets(nil)
	awaitUpdate(t, updates, func(u update) bool { return u.synced && len(u.certificates) == 0 })
	writePEM(t, chain2, cert)
	time.Sleep(700 * time.Millisecond)
	select {
	case u := <-updates:
		t.Fatalf("deleted secret published again: %+v", u.certificates)
	default:
	}
}

func TestFileSecretsAreIndependent(t *testing.T) {
	cert, key := testCertificate(t)
	otherCert, otherKey := testCertificate(t)
	good := t.TempDir()
	bad := filepath.Join(t.TempDir(), "later")
	goodChain, goodKey := filepath.Join(good, "fullchain.pem"), filepath.Join(good, "key.pem")
	badChain, badKey := filepath.Join(bad, "fullchain.pem"), filepath.Join(bad, "key.pem")
	writePEM(t, goodChain, cert)
	writePEM(t, goodKey, key)
	updates := make(chan update, 100)
	m, err := New(nil, []config.Secret{fileSecret("good", goodChain, goodKey), fileSecret("bad", badChain, badKey)}, slog.New(slog.NewTextHandler(io.Discard, nil)), func(cs []Certificate, synced bool) {
		updates <- update{cs, synced}
	})
	if err != nil {
		t.Fatal(err)
	}
	startManager(t, m)
	u := awaitUpdate(t, updates, func(u update) bool { return u.synced && len(u.certificates) == 1 })
	if u.certificates[0].Name != "good" {
		t.Fatal("failed secret affected the other one")
	}
	writePEM(t, badChain, otherCert)
	writePEM(t, badKey, otherKey)
	awaitUpdate(t, updates, func(u update) bool { return u.synced && len(u.certificates) == 2 })
}

func TestFileSourceDoesNotLogOrFetch(t *testing.T) {
	cert, key := testCertificate(t)
	dir := t.TempDir()
	chain, keyPath := filepath.Join(dir, "fullchain.pem"), filepath.Join(dir, "key.pem")
	writePEM(t, chain, cert)
	const marker = "SUPER-SECRET-KEY-MATERIAL"
	writePEM(t, keyPath, marker)
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer server.Close()
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	updates := make(chan update, 100)
	m, err := New(&config.OpenBao{Address: server.URL, TokenFile: token, RefreshInterval: "20ms"}, []config.Secret{fileSecret("cert", chain, keyPath)}, log, func(cs []Certificate, synced bool) {
		updates <- update{cs, synced}
	})
	if err != nil {
		t.Fatal(err)
	}
	startManager(t, m)
	awaitUpdate(t, updates, func(u update) bool { return u.synced && len(u.certificates) == 0 })
	time.Sleep(150 * time.Millisecond)
	if calls.Load() != 0 {
		t.Fatal("file source requested OpenBao")
	}
	if strings.Contains(buf.String(), marker) || strings.Contains(buf.String(), key) {
		t.Fatal("log contains certificate or private key material")
	}
	writePEM(t, keyPath, key)
	awaitUpdate(t, updates, func(u update) bool { return u.synced && len(u.certificates) == 1 })
	if calls.Load() != 0 || strings.Contains(buf.String(), key) || strings.Contains(buf.String(), marker) {
		t.Fatal("publishing a file certificate used HTTP or logged key material")
	}
}
