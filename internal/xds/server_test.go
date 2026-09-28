package xds

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	route "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	discovery "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// TestServeOverUDS drives the server like Envoy would: an ADS stream asking
// for the RouteConfiguration of one of the two entrypoints. It must be
// answered even though Envoy did not name every RouteConfiguration.
func TestServeOverUDS(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sock := filepath.Join(t.TempDir(), "xds.sock")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	snapshots := cache.NewSnapshotCache(false, cache.IDHash{}, nil)
	res := Translate(testModel(), nil)
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
