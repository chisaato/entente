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
	"github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/envoyproxy/go-control-plane/pkg/server/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
)

// Server serves xDS (ADS plus the individual CDS/EDS/RDS services) over a
// Unix domain socket. File permissions on the socket are the trust boundary.
type Server struct {
	grpc   *grpc.Server
	socket string
	mode   os.FileMode
	log    *slog.Logger
}

// NewServer wires a go-control-plane server on top of snapshots. nodeID is
// only used to warn about Envoys that connect with a different node.id and
// would therefore never receive configuration. RDS resource names Envoy
// requests are reported to routeNames.
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

	return &Server{grpc: g, socket: socket, mode: mode, log: log}
}

// Serve listens on the socket and blocks until Stop is called or serving fails.
func (s *Server) Serve() error {
	if err := removeStaleSocket(s.socket); err != nil {
		return err
	}
	// Create the socket with no group/other access, then widen to the
	// configured mode, so it is never briefly more permissive than intended.
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

// Stop closes the listener and all streams.
func (s *Server) Stop() { s.grpc.Stop() }

// removeStaleSocket deletes a leftover socket from a previous run. Anything
// that is not a socket is left alone and reported.
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
