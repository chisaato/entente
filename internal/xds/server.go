package xds

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"syscall"
	"time"

	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	clusterservice "github.com/envoyproxy/go-control-plane/envoy/service/cluster/v3"
	discovery "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	endpointservice "github.com/envoyproxy/go-control-plane/envoy/service/endpoint/v3"
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
	grpc   *grpc.Server
	socket string
	mode   os.FileMode
	log    *slog.Logger
}

// NewServer 在 snapshots 之上接好 go-control-plane 的 server。nodeID 只用于警告
// 那些以不同 node.id 连上来、因而永远收不到配置的 Envoy。
// Envoy 请求的 RDS 资源名会报告给 routeNames。
func NewServer(ctx context.Context, snapshots cache.SnapshotCache, routeNames *RouteNames, nodeID, socket string, mode os.FileMode, log *slog.Logger) *Server {
	cb := server.CallbackFuncs{
		StreamOpenFunc: func(_ context.Context, id int64, typeURL string) error {
			log.Debug("xds stream open", "stream", id, "type", typeURL)
			return nil
		},
		StreamClosedFunc: func(id int64, node *core.Node) {
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
			if st := req.GetErrorDetail(); st != nil {
				log.Error("envoy rejected configuration (NACK)",
					"type", req.GetTypeUrl(), "version", req.GetVersionInfo(), "nonce", req.GetResponseNonce(), "error", st.GetMessage())
			}
			return nil
		},
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

	return &Server{grpc: g, socket: socket, mode: mode, log: log}
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
