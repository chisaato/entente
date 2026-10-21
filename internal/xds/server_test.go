package xds

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/chisaato/entente/internal/config"
	"github.com/chisaato/entente/internal/secrets"
	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	route "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	discovery "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// TestServeOverUDS 像 Envoy 那样驱动 server:一条 ADS 流只请求两个 entrypoint 之一的
// RouteConfiguration。即使 Envoy 没有点名每一份 RouteConfiguration,也必须得到应答。
func TestServeOverUDS(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sock := filepath.Join(t.TempDir(), "xds.sock")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	snapshots := cache.NewSnapshotCache(false, cache.IDHash{}, nil)
	res := Translate(testModel(), nil, []secrets.Certificate{{Name: "example-cert", Certificate: []byte("chain"), PrivateKey: []byte("key")}})
	v, _ := res.Version()
	snap, err := res.Snapshot(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := snapshots.SetSnapshot(ctx, "envoy", snap); err != nil {
		t.Fatal(err)
	}

	routeNames := NewRouteNames(nil)
	srv := NewServer(ctx, snapshots, routeNames, "envoy", sock, 0o660, log)
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve() }()
	defer srv.Stop()

	var fi os.FileInfo
	for deadline := time.Now().Add(5 * time.Second); ; {
		if fi, err = os.Stat(sock); err == nil && fi.Mode().Perm() == 0o660 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("socket not ready: fi=%v err=%v", fi, err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	conn, err := grpc.NewClient("unix://"+sock, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	stream, err := discovery.NewAggregatedDiscoveryServiceClient(conn).StreamAggregatedResources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&discovery.DiscoveryRequest{
		Node:          &core.Node{Id: "envoy"},
		TypeUrl:       resource.RouteType,
		ResourceNames: []string{"https"},
	}); err != nil {
		t.Fatal(err)
	}
	resp, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetVersionInfo() != v || len(resp.GetResources()) != 1 {
		t.Fatalf("response version=%s resources=%d", resp.GetVersionInfo(), len(resp.GetResources()))
	}
	var rc route.RouteConfiguration
	if err := resp.GetResources()[0].UnmarshalTo(&rc); err != nil {
		t.Fatal(err)
	}
	if rc.GetName() != "https" || rc.GetVirtualHosts()[0].GetDomains()[0] != "whoami.example.com" {
		t.Errorf("route config = %v", &rc)
	}

	if got := routeNames.Names(); len(got) != 1 || got[0] != "https" {
		t.Errorf("observed route names = %v", got)
	}

	// SDS 和 RDS 共用同一条 ADS 流。
	if err := stream.Send(&discovery.DiscoveryRequest{Node: &core.Node{Id: "envoy"}, TypeUrl: resource.SecretType, ResourceNames: []string{"example-cert"}}); err != nil {
		t.Fatal(err)
	}
	sds, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if sds.GetTypeUrl() != resource.SecretType || sds.GetVersionInfo() != v || len(sds.GetResources()) != 1 {
		t.Fatal("invalid SDS response")
	}
	var secret tlsv3.Secret
	if err := sds.GetResources()[0].UnmarshalTo(&secret); err != nil {
		t.Fatal(err)
	}
	if secret.GetName() != "example-cert" || string(secret.GetTlsCertificate().GetCertificateChain().GetInlineBytes()) != "chain" || string(secret.GetTlsCertificate().GetPrivateKey().GetInlineBytes()) != "key" {
		t.Fatal("invalid SDS certificate")
	}

	srv.Stop()
	if err := <-errc; err != nil {
		t.Errorf("Serve returned %v after Stop", err)
	}
}

func pemPair(t *testing.T) (string, string) {
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
	encoded, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded}))
}

// TestFileCertificateRotationOverUDS 用和 Envoy 一样的 ADS SDS 流,确认文件轮换后
// 资源里的证书字节变了,而不是只调用了回调。
func TestFileCertificateRotationOverUDS(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	dir := t.TempDir()
	chainPath, keyPath := filepath.Join(dir, "fullchain.pem"), filepath.Join(dir, "key.pem")
	cert, key := pemPair(t)
	if err := os.WriteFile(chainPath, []byte(cert), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte(key), 0o600); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(t.TempDir(), "xds.sock")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	snapshots := cache.NewSnapshotCache(false, cache.IDHash{}, nil)
	published := make(chan string, 4)
	manager, err := secrets.New(nil, []config.Secret{{Name: "example-cert", File: &config.SecretFile{CertificateChain: chainPath, PrivateKey: keyPath}}}, log, func(cs []secrets.Certificate, synced bool) {
		if !synced {
			return
		}
		res := Translate(testModel(), nil, cs)
		version, err := res.Version()
		if err != nil {
			return
		}
		snap, err := res.Snapshot(version)
		if err != nil {
			return
		}
		if err := snapshots.SetSnapshot(context.Background(), "envoy", snap); err != nil {
			return
		}
		got := ""
		if len(cs) == 1 {
			got = string(cs[0].Certificate)
		}
		select {
		case published <- got:
		default:
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	routeNames := NewRouteNames(nil)
	srv := NewServer(ctx, snapshots, routeNames, "envoy", sock, 0o600, log)
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve() }()
	defer srv.Stop()
	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()
	go manager.Run(runCtx)

	waitChain := func(want string) {
		t.Helper()
		timer := time.NewTimer(10 * time.Second)
		defer timer.Stop()
		for {
			select {
			case got := <-published:
				if got == want {
					return
				}
			case <-timer.C:
				t.Fatal("timed out waiting for snapshot")
			}
		}
	}
	waitChain(cert)
	for deadline := time.Now().Add(5 * time.Second); ; {
		if fi, err := os.Stat(sock); err == nil && fi.Mode().Perm() == 0o600 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("socket not ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	conn, err := grpc.NewClient("unix://"+sock, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	stream, err := discovery.NewAggregatedDiscoveryServiceClient(conn).StreamAggregatedResources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	node := &core.Node{Id: "envoy"}
	if err := stream.Send(&discovery.DiscoveryRequest{Node: node, TypeUrl: resource.SecretType, ResourceNames: []string{"example-cert"}}); err != nil {
		t.Fatal(err)
	}
	recvChain := func() (string, *discovery.DiscoveryResponse) {
		t.Helper()
		resp, err := stream.Recv()
		if err != nil {
			t.Fatal(err)
		}
		if resp.GetTypeUrl() != resource.SecretType || len(resp.GetResources()) != 1 {
			t.Fatalf("SDS response type=%s resources=%d", resp.GetTypeUrl(), len(resp.GetResources()))
		}
		var secret tlsv3.Secret
		if err := resp.GetResources()[0].UnmarshalTo(&secret); err != nil {
			t.Fatal(err)
		}
		return string(secret.GetTlsCertificate().GetCertificateChain().GetInlineBytes()), resp
	}
	got, resp := recvChain()
	if got != cert {
		t.Fatal("SDS resource does not contain the file certificate")
	}
	firstVersion := resp.GetVersionInfo()
	if err := stream.Send(&discovery.DiscoveryRequest{Node: node, TypeUrl: resource.SecretType, ResourceNames: []string{"example-cert"}, VersionInfo: resp.GetVersionInfo(), ResponseNonce: resp.GetNonce()}); err != nil {
		t.Fatal(err)
	}
	rotated, rotatedKey := pemPair(t)
	if err := os.WriteFile(chainPath, []byte(rotated), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte(rotatedKey), 0o600); err != nil {
		t.Fatal(err)
	}
	waitChain(rotated)
	got, resp = recvChain()
	if got != rotated || resp.GetVersionInfo() == firstVersion {
		t.Fatal("SDS resource was not rotated")
	}
	runCancel()
	srv.Stop()
	if err := <-errc; err != nil {
		t.Errorf("Serve returned %v after Stop", err)
	}
}

func TestRemoveStaleSocketRefusesRegularFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "not-a-socket")
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeStaleSocket(p); err == nil {
		t.Fatal("expected an error for a regular file")
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatal("regular file was removed")
	}
}
