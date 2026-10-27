package xds

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"sync"
	"syscall"
	"time"

	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	clusterservice "github.com/envoyproxy/go-control-plane/envoy/service/cluster/v3"
	discovery "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	endpointservice "github.com/envoyproxy/go-control-plane/envoy/service/endpoint/v3"
	listenerservice "github.com/envoyproxy/go-control-plane/envoy/service/listener/v3"
	routeservice "github.com/envoyproxy/go-control-plane/envoy/service/route/v3"
	secretservice "github.com/envoyproxy/go-control-plane/envoy/service/secret/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/envoyproxy/go-control-plane/pkg/server/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
)

// Server 通过 Unix domain socket 提供 xDS(ADS 以及单独的 CDS/EDS/RDS 服务)。
// socket 上的文件权限就是信任边界。
type Server struct {
	grpc         *grpc.Server
	socket       string
	mode         os.FileMode
	log          *slog.Logger
	snapshots    cache.SnapshotCache
	nodeID       string
	observations *streamObservations
}

// NewServer 在 snapshots 之上接好 go-control-plane 的 server。nodeID 只用于警告
// 那些以不同 node.id 连上来、因而永远收不到配置的 Envoy。
// Envoy 请求的 RDS 资源名会报告给 routeNames。
func NewServer(ctx context.Context, snapshots cache.SnapshotCache, routeNames *RouteNames, nodeID, socket string, mode os.FileMode, log *slog.Logger) *Server {
	observations := &streamObservations{log: log, pending: make(map[int64]map[responseKey]string)}
	cb := server.CallbackFuncs{
		StreamOpenFunc: func(_ context.Context, id int64, typeURL string) error {
			log.Debug("xds stream open", "stream", id, "type", typeURL)
			return nil
		},
		StreamClosedFunc: func(id int64, node *core.Node) {
			observations.close(id)
			log.Debug("xds stream closed", "stream", id, "node", node.GetId())
		},
		StreamRequestFunc: func(id int64, req *discovery.DiscoveryRequest) error {
			if n := req.GetNode().GetId(); n != "" && n != nodeID {
				log.Warn("xds request from unexpected node; it will not receive configuration",
					"stream", id, "node", n, "expected", nodeID)
			}
			if req.GetTypeUrl() == resource.RouteType {
				routeNames.Observe(req.GetResourceNames())
			}
			observations.request(id, req)
			return nil
		},
		StreamResponseFunc: observations.response,
	}
	xdsServer := server.NewServer(ctx, snapshots, cb)

	g := grpc.NewServer(
		grpc.MaxConcurrentStreams(1000),
		grpc.KeepaliveParams(keepalive.ServerParameters{Time: 30 * time.Second, Timeout: 5 * time.Second}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 30 * time.Second, PermitWithoutStream: true}),
	)
	discovery.RegisterAggregatedDiscoveryServiceServer(g, xdsServer)
	clusterservice.RegisterClusterDiscoveryServiceServer(g, xdsServer)
	endpointservice.RegisterEndpointDiscoveryServiceServer(g, xdsServer)
	routeservice.RegisterRouteDiscoveryServiceServer(g, xdsServer)
	secretservice.RegisterSecretDiscoveryServiceServer(g, xdsServer)
	listenerservice.RegisterListenerDiscoveryServiceServer(g, xdsServer)

	return &Server{grpc: g, socket: socket, mode: mode, log: log, snapshots: snapshots, nodeID: nodeID, observations: observations}
}

// SetListenerAcknowledged 注入 LDS 确认回调,避免 xDS 依赖协调器。
func (s *Server) SetListenerAcknowledged(callback func(string)) {
	s.observations.mu.Lock()
	defer s.observations.mu.Unlock()
	s.observations.listenerAcknowledged = callback
}

// Publish 是发布回调入口:调用方可把 srv.Publish 注入发布流程。
// generationID 是资源内容哈希,必须同时用作快照内所有资源类型的版本。
// published 仅表示快照已交给缓存,不表示 Envoy 已 ACK;NACK 不触发回滚。
func (s *Server) Publish(ctx context.Context, generationID string, snapshot cache.ResourceSnapshot) error {
	if generationID == "" {
		return errors.New("generation id must not be empty")
	}
	for _, typeURL := range []string{resource.ClusterType, resource.EndpointType, resource.RouteType, resource.SecretType, resource.ListenerType} {
		if version := snapshot.GetVersion(typeURL); version != "" && version != generationID {
			return fmt.Errorf("snapshot %s version %q differs from generation %q", typeURL, version, generationID)
		}
	}
	if err := s.snapshots.SetSnapshot(ctx, s.nodeID, snapshot); err != nil {
		return err
	}
	s.log.Info("xds snapshot published", "stage", "published", "generation_id", generationID, "version", generationID, "node", s.nodeID)
	return nil
}

type responseKey struct {
	typeURL string
	nonce   string
}

// streamObservations 只保留尚未收到确认的响应,关闭流时释放所有关联。
type streamObservations struct {
	mu                   sync.Mutex
	log                  *slog.Logger
	pending              map[int64]map[responseKey]string
	listenerAcknowledged func(string)
}

func (o *streamObservations) response(_ context.Context, stream int64, _ *discovery.DiscoveryRequest, resp *discovery.DiscoveryResponse) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.pending[stream] == nil {
		o.pending[stream] = make(map[responseKey]string)
	}
	o.pending[stream][responseKey{resp.GetTypeUrl(), resp.GetNonce()}] = resp.GetVersionInfo()
}

func (o *streamObservations) close(stream int64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.pending, stream)
}

func (o *streamObservations) request(stream int64, req *discovery.DiscoveryRequest) {
	if req.GetResponseNonce() == "" {
		return
	}
	key := responseKey{req.GetTypeUrl(), req.GetResponseNonce()}
	o.mu.Lock()
	version, found := o.pending[stream][key]
	acknowledged := o.listenerAcknowledged
	delete(o.pending[stream], key)
	o.mu.Unlock()
	if !found {
		// 未知 nonce 不能冒充某一代的确认,尤其不能借用请求里的旧版本。
		if st := req.GetErrorDetail(); st != nil {
			o.log.Error("envoy rejected configuration with no matching response (NACK)",
				"stage", "nack_unmatched", "stream", stream, "type", key.typeURL, "nonce", key.nonce,
				"accepted_version", req.GetVersionInfo(), "error", st.GetMessage(), "error_detail", st)
			return
		}
		o.log.Warn("xds confirmation has no matching response", "stream", stream, "type", key.typeURL, "nonce", key.nonce)
		return
	}
	attrs := []any{"stream", stream, "type", key.typeURL, "nonce", key.nonce, "generation_id", version, "version", version, "accepted_version", req.GetVersionInfo()}
	if st := req.GetErrorDetail(); st != nil {
		attrs = append(attrs, "stage", "nack", "error", st.GetMessage(), "error_detail", st)
		o.log.Error("envoy rejected configuration (NACK)", attrs...)
		return
	}
	if req.GetVersionInfo() != version {
		o.log.Warn("xds confirmation version does not match response", attrs...)
		return
	}
	attrs = append(attrs, "stage", "ack")
	o.log.Info("envoy accepted configuration (ACK)", attrs...)
	if key.typeURL == resource.ListenerType && acknowledged != nil {
		acknowledged(version)
	}
}

// Serve 在 socket 上监听,阻塞到 Stop 被调用或服务失败。
func (s *Server) Serve() error {
	if err := removeStaleSocket(s.socket); err != nil {
		return err
	}
	// 创建 socket 时先去掉 group/other 权限,再放宽到配置的 mode,
	// 这样它不会短暂地比预期更宽松。
	old := syscall.Umask(0o177)
	lis, err := net.Listen("unix", s.socket)
	syscall.Umask(old)
	if err != nil {
		return err
	}
	if err := os.Chmod(s.socket, s.mode); err != nil {
		lis.Close()
		return fmt.Errorf("chmod %s: %w", s.socket, err)
	}
	s.log.Info("xds server listening", "socket", s.socket, "mode", fmt.Sprintf("%#o", s.mode))
	return s.grpc.Serve(lis)
}

// Stop 关闭 listener 和所有流。
func (s *Server) Stop() { s.grpc.Stop() }

// removeStaleSocket 删除上一次运行留下的 socket。不是 socket 的路径原样保留并报错。
func removeStaleSocket(path string) error {
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if fi.Mode()&fs.ModeSocket == 0 {
		return fmt.Errorf("%s exists and is not a socket; refusing to remove it", path)
	}
	return os.Remove(path)
}
