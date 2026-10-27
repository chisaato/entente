package xds

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/chisaato/entente/internal/secrets"
	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	discovery "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	status "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type publicationLogs struct {
	slog.Handler
	records chan slog.Record
}

func (h *publicationLogs) Handle(_ context.Context, record slog.Record) error {
	h.records <- record.Clone()
	return nil
}

type failingPublicationCache struct {
	cache.SnapshotCache
	err error
}

func (c failingPublicationCache) SetSnapshot(context.Context, string, cache.ResourceSnapshot) error {
	return c.err
}

func TestPublishFailureDoesNotReportPublished(t *testing.T) {
	res := Translate(testModel(), nil, nil)
	version, err := res.Version()
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := res.Snapshot(version)
	if err != nil {
		t.Fatal(err)
	}
	handler := &publicationLogs{Handler: slog.NewTextHandler(io.Discard, nil), records: make(chan slog.Record, 4)}
	wantErr := errors.New("cache unavailable")
	srv := &Server{snapshots: failingPublicationCache{err: wantErr}, nodeID: "envoy", log: slog.New(handler)}
	if err := srv.Publish(context.Background(), version, snapshot); !errors.Is(err, wantErr) {
		t.Fatalf("Publish error = %v", err)
	}
	for _, generation := range []string{"", "not-the-snapshot-version"} {
		if err := srv.Publish(context.Background(), generation, snapshot); err == nil || errors.Is(err, wantErr) {
			t.Fatalf("invalid generation %q reached cache: %v", generation, err)
		}
	}
	select {
	case record := <-handler.records:
		t.Fatalf("failed publication produced a log: %v", record.Message)
	default:
	}
}

func TestListenerAcknowledgedOnlyForMatchingLDSACK(t *testing.T) {
	var versions []string
	o := &streamObservations{
		log:                  slog.New(slog.NewTextHandler(io.Discard, nil)),
		pending:              make(map[int64]map[responseKey]string),
		listenerAcknowledged: func(version string) { versions = append(versions, version) },
	}
	for _, tc := range []struct {
		typeURL, nonce, accepted string
		stream                   int64
		nack                     bool
	}{
		{resource.ListenerType, "valid", "candidate", 1, false},
		{resource.RouteType, "route", "candidate", 1, false},
		{resource.ListenerType, "nack", "old", 1, true},
		{resource.ListenerType, "wrong-version", "old", 1, false},
		{resource.ListenerType, "wrong-stream", "candidate", 2, false},
	} {
		o.response(context.Background(), 1, nil, &discovery.DiscoveryResponse{TypeUrl: tc.typeURL, Nonce: tc.nonce, VersionInfo: "candidate"})
		req := &discovery.DiscoveryRequest{TypeUrl: tc.typeURL, ResponseNonce: tc.nonce, VersionInfo: tc.accepted}
		if tc.nack {
			req.ErrorDetail = &status.Status{Code: 3, Message: "invalid listener"}
		}
		o.request(tc.stream, req)
		// 已消费的确认不能重复触发回调。
		o.request(tc.stream, req)
	}
	if len(versions) != 1 || versions[0] != "candidate" {
		t.Fatalf("listener acknowledgments=%v", versions)
	}
}

func TestPublicationACKNACKOverUDS(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	handler := &publicationLogs{Handler: slog.NewTextHandler(io.Discard, nil), records: make(chan slog.Record, 64)}
	snapshots := cache.NewSnapshotCache(false, cache.IDHash{}, nil)
	socket := filepath.Join(t.TempDir(), "xds.sock")
	srv := NewServer(ctx, snapshots, NewRouteNames(nil), "envoy", socket, 0o600, slog.New(handler))
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve() }()
	t.Cleanup(func() {
		srv.Stop()
		if err := <-errCh; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	for {
		if fi, err := os.Stat(socket); err == nil && fi.Mode().Perm() == 0o600 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("socket not ready")
		case <-time.After(10 * time.Millisecond):
		}
	}

	waitLog := func(stage string) (slog.Record, map[string]any) {
		t.Helper()
		for {
			select {
			case record := <-handler.records:
				attrs := make(map[string]any)
				record.Attrs(func(a slog.Attr) bool { attrs[a.Key] = a.Value.Any(); return true })
				if attrs["stage"] == stage {
					return record, attrs
				}
			case <-ctx.Done():
				t.Fatalf("missing %s log", stage)
			}
		}
	}
	publish := func(chain string) string {
		t.Helper()
		res := Translate(testModel(), nil, []secrets.Certificate{{Name: "cert", Certificate: []byte(chain), PrivateKey: []byte("key")}})
		version, err := res.Version()
		if err != nil {
			t.Fatal(err)
		}
		snapshot, err := res.Snapshot(version)
		if err != nil {
			t.Fatal(err)
		}
		if err := srv.Publish(ctx, version, snapshot); err != nil {
			t.Fatal(err)
		}
		_, attrs := waitLog("published")
		if attrs["generation_id"] != version {
			t.Fatalf("published attributes = %v", attrs)
		}
		return version
	}
	first := publish("first")
	conn, err := grpc.NewClient("unix://"+socket, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := discovery.NewAggregatedDiscoveryServiceClient(conn)
	open := func() discovery.AggregatedDiscoveryService_StreamAggregatedResourcesClient {
		t.Helper()
		stream, err := client.StreamAggregatedResources(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return stream
	}
	send := func(stream discovery.AggregatedDiscoveryService_StreamAggregatedResourcesClient, req *discovery.DiscoveryRequest) {
		t.Helper()
		req.Node = &core.Node{Id: "envoy"}
		if err := stream.Send(req); err != nil {
			t.Fatal(err)
		}
	}
	recv := func(stream discovery.AggregatedDiscoveryService_StreamAggregatedResourcesClient) *discovery.DiscoveryResponse {
		t.Helper()
		resp, err := stream.Recv()
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	ack := func(stream discovery.AggregatedDiscoveryService_StreamAggregatedResourcesClient, resp *discovery.DiscoveryResponse, names []string) map[string]any {
		t.Helper()
		send(stream, &discovery.DiscoveryRequest{TypeUrl: resp.GetTypeUrl(), ResourceNames: names, VersionInfo: resp.GetVersionInfo(), ResponseNonce: resp.GetNonce()})
		_, attrs := waitLog("ack")
		if attrs["generation_id"] != resp.GetVersionInfo() || attrs["version"] != resp.GetVersionInfo() || attrs["nonce"] != resp.GetNonce() || attrs["type"] != resp.GetTypeUrl() {
			t.Fatalf("ACK attributes = %v, response = %v", attrs, resp)
		}
		return attrs
	}

	// 不同流会重复使用 nonce,不能只按 nonce 关联。
	stream1, stream2 := open(), open()
	for _, stream := range []discovery.AggregatedDiscoveryService_StreamAggregatedResourcesClient{stream1, stream2} {
		send(stream, &discovery.DiscoveryRequest{TypeUrl: resource.RouteType, ResourceNames: []string{"https"}})
	}
	route1, route2 := recv(stream1), recv(stream2)
	if route1.GetNonce() != route2.GetNonce() {
		t.Fatal("expected overlapping stream-local nonces")
	}
	a1 := ack(stream1, route1, []string{"https"})
	a2 := ack(stream2, route2, []string{"https"})
	if a1["stream"] == a2["stream"] {
		t.Fatal("different streams were conflated")
	}

	// 另一类型携带同一个 nonce 时,不能消费 RDS 的关联。
	send(stream1, &discovery.DiscoveryRequest{TypeUrl: resource.SecretType, ResourceNames: []string{"cert"}})
	secret1 := recv(stream1)
	ack(stream1, secret1, []string{"cert"})
	second := publish("second")
	if first == second {
		t.Fatal("snapshot content hash did not change")
	}
	responses := map[string]*discovery.DiscoveryResponse{}
	for range 2 {
		resp := recv(stream1)
		responses[resp.GetTypeUrl()] = resp
	}
	rejected := responses[resource.RouteType]
	if rejected == nil || responses[resource.SecretType] == nil {
		t.Fatalf("missing response types: %v", responses)
	}
	send(stream1, &discovery.DiscoveryRequest{TypeUrl: resource.SecretType, ResourceNames: []string{"cert"}, VersionInfo: first, ResponseNonce: rejected.GetNonce(), ErrorDetail: &status.Status{Code: 3, Message: "wrong type nonce"}})
	send(stream1, &discovery.DiscoveryRequest{TypeUrl: resource.RouteType, ResourceNames: []string{"https"}, VersionInfo: first, ResponseNonce: rejected.GetNonce(), ErrorDetail: &status.Status{Code: 3, Message: "invalid route from envoy"}})
	record, attrs := waitLog("nack")
	if record.Level != slog.LevelError || attrs["generation_id"] != second || attrs["version"] != second || attrs["accepted_version"] != first || attrs["type"] != resource.RouteType || attrs["nonce"] != rejected.GetNonce() || attrs["stream"] != a1["stream"] || attrs["error"] != "invalid route from envoy" {
		t.Fatalf("NACK level=%v attributes=%v", record.Level, attrs)
	}
	if detail, ok := attrs["error_detail"].(*status.Status); !ok || detail.GetCode() != 3 {
		t.Fatalf("missing Envoy error_detail: %v", attrs)
	}
	ack(stream1, responses[resource.SecretType], []string{"cert"})

	// NACK 不回滚缓存,新流仍收到刚被拒绝的生成代。
	current, err := snapshots.GetSnapshot("envoy")
	if err != nil || current.GetVersion(resource.RouteType) != second {
		t.Fatalf("snapshot changed after NACK: %v, %v", current, err)
	}
	stream3 := open()
	send(stream3, &discovery.DiscoveryRequest{TypeUrl: resource.RouteType, ResourceNames: []string{"https"}})
	if resp := recv(stream3); resp.GetVersionInfo() != second {
		t.Fatalf("new stream got rolled-back version %s", resp.GetVersionInfo())
	}
}
