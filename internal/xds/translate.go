// Package xds turns a model.Model into Envoy xDS resources and serves them.
package xds

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"slices"
	"time"

	cluster "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpoint "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	route "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	"github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/chisaato/entente/internal/model"
)

// upstreamConnectTimeout is set explicitly on every generated cluster rather
// than relying on Envoy's built-in default.
const upstreamConnectTimeout = 5 * time.Second

// adsSource points a resource at the ADS stream configured in the bootstrap.
func adsSource() *core.ConfigSource {
	return &core.ConfigSource{
		ResourceApiVersion:    core.ApiVersion_V3,
		ConfigSourceSpecifier: &core.ConfigSource_Ads{Ads: &core.AggregatedConfigSource{}},
	}
}

// Resources holds the xDS resources for one snapshot, in deterministic order.
type Resources struct {
	Clusters  []types.Resource
	Endpoints []types.Resource
	Routes    []types.Resource
}

// Translate builds CDS, EDS and RDS resources from m. The model must already
// be validated and sorted (reconcile.Build guarantees both).
//
// Every declared entrypoint gets a RouteConfiguration, even without routes, so
// Listeners referencing it via RDS never sit waiting for a first response.
// Each name in placeholders that is not a declared entrypoint gets an empty
// RouteConfiguration, so listeners Envoy has but the config does not declare
// serve nothing (404) instead of stale routes.
func Translate(m *model.Model, placeholders []string) Resources {
	var res Resources

	for _, b := range m.Backends {
		res.Clusters = append(res.Clusters, &cluster.Cluster{
			Name:                 b.Name,
			ClusterDiscoveryType: &cluster.Cluster_Type{Type: cluster.Cluster_EDS},
			EdsClusterConfig: &cluster.Cluster_EdsClusterConfig{
				EdsConfig:   adsSource(),
				ServiceName: b.Name,
			},
			ConnectTimeout: durationpb.New(upstreamConnectTimeout),
			LbPolicy:       cluster.Cluster_ROUND_ROBIN,
		})

		lbs := make([]*endpoint.LbEndpoint, 0, len(b.Endpoints))
		for _, ep := range b.Endpoints {
			lbs = append(lbs, &endpoint.LbEndpoint{
				HostIdentifier: &endpoint.LbEndpoint_Endpoint{Endpoint: &endpoint.Endpoint{
					Address: &core.Address{Address: &core.Address_SocketAddress{SocketAddress: &core.SocketAddress{
						Protocol:      core.SocketAddress_TCP,
						Address:       ep.Addr().String(),
						PortSpecifier: &core.SocketAddress_PortValue{PortValue: uint32(ep.Port())},
					}}},
				}},
			})
		}
		res.Endpoints = append(res.Endpoints, &endpoint.ClusterLoadAssignment{
			ClusterName: b.Name,
			Endpoints:   []*endpoint.LocalityLbEndpoints{{LbEndpoints: lbs}},
		})
	}

	for _, ep := range m.Entrypoints {
		rc := &route.RouteConfiguration{Name: ep}
		for _, r := range m.Routes {
			if !slices.Contains(r.Entrypoints, ep) {
				continue
			}
			rc.VirtualHosts = append(rc.VirtualHosts, &route.VirtualHost{
				Name:    r.Name,
				Domains: []string{r.Host},
				Routes: []*route.Route{{
					Match: &route.RouteMatch{PathSpecifier: &route.RouteMatch_Prefix{Prefix: "/"}},
					Action: &route.Route_Route{Route: &route.RouteAction{
						ClusterSpecifier: &route.RouteAction_Cluster{Cluster: r.Backend},
					}},
				}},
			})
		}
		res.Routes = append(res.Routes, rc)
	}
	for _, name := range placeholders {
		if !slices.Contains(m.Entrypoints, name) {
			res.Routes = append(res.Routes, &route.RouteConfiguration{Name: name})
		}
	}

	return res
}

// Version is a content hash of the resources. Using content rather than a
// clock or counter means an unchanged model yields an unchanged version, and
// a restarted control plane reproduces the version Envoy already has.
func (r Resources) Version() (string, error) {
	h := sha256.New()
	opts := proto.MarshalOptions{Deterministic: true}
	for _, group := range [][]types.Resource{r.Clusters, r.Endpoints, r.Routes} {
		for _, res := range group {
			b, err := opts.Marshal(res)
			if err != nil {
				return "", err
			}
			// Length-prefix each message so concatenation is unambiguous.
			h.Write(binary.LittleEndian.AppendUint64(nil, uint64(len(b))))
			h.Write(b)
		}
		h.Write([]byte{0xff}) // group separator
	}
	return hex.EncodeToString(h.Sum(nil))[:16], nil
}

// Snapshot builds a consistent cache snapshot for the resources.
func (r Resources) Snapshot(version string) (*cache.Snapshot, error) {
	snap, err := cache.NewSnapshot(version, map[resource.Type][]types.Resource{
		resource.ClusterType:  r.Clusters,
		resource.EndpointType: r.Endpoints,
		resource.RouteType:    r.Routes,
	})
	if err != nil {
		return nil, err
	}
	if err := r.consistent(); err != nil {
		return nil, err
	}
	return snap, nil
}

// consistent checks cross-references within the snapshot. The upstream
// Snapshot.Consistent is not usable here: it expects every RouteConfiguration
// to be referenced by an LDS listener, but our listeners are static.
func (r Resources) consistent() error {
	clusters := map[string]bool{}
	for _, res := range r.Clusters {
		c := res.(*cluster.Cluster)
		clusters[c.GetName()] = true
		if c.GetEdsClusterConfig().GetServiceName() != c.GetName() {
			return fmt.Errorf("cluster %q: EDS service name %q differs from cluster name", c.GetName(), c.GetEdsClusterConfig().GetServiceName())
		}
	}
	assignments := map[string]bool{}
	for _, res := range r.Endpoints {
		name := res.(*endpoint.ClusterLoadAssignment).GetClusterName()
		if !clusters[name] {
			return fmt.Errorf("endpoints for unknown cluster %q", name)
		}
		assignments[name] = true
	}
	for name := range clusters {
		if !assignments[name] {
			return fmt.Errorf("cluster %q has no endpoints resource", name)
		}
	}
	for _, res := range r.Routes {
		rc := res.(*route.RouteConfiguration)
		domains := map[string]bool{}
		for _, vh := range rc.GetVirtualHosts() {
			for _, d := range vh.GetDomains() {
				if domains[d] {
					return fmt.Errorf("route config %q: domain %q appears twice", rc.GetName(), d)
				}
				domains[d] = true
			}
			for _, rt := range vh.GetRoutes() {
				if c := rt.GetRoute().GetCluster(); !clusters[c] {
					return fmt.Errorf("route config %q: virtual host %q references unknown cluster %q", rc.GetName(), vh.GetName(), c)
				}
			}
		}
	}
	return nil
}
