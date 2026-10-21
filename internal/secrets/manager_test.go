package secrets

import (
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
	"sync"
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
	m, err := New(config.OpenBao{Address: server.URL, TokenFile: path, RefreshInterval: "20ms"}, []config.Secret{testDefinition("cert")}, slog.New(slog.NewTextHandler(io.Discard, nil)), callback)
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
