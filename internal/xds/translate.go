// Package xds 把 model.Model 变成 Envoy xDS 资源并提供服务。
package xds

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"

	cluster "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpoint "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	route "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	"github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/chisaato/entente/internal/model"
	"github.com/chisaato/entente/internal/secrets"
)

// upstreamConnectTimeout 显式设置在每个生成的 cluster 上,
// 而不是依赖 Envoy 的内置默认值。
const upstreamConnectTimeout = 5 * time.Second

// adsSource 让资源指向 bootstrap 里配置的 ADS 流。
func adsSource() *core.ConfigSource {
	return &core.ConfigSource{
		ResourceApiVersion:    core.ApiVersion_V3,
		ConfigSourceSpecifier: &core.ConfigSource_Ads{Ads: &core.AggregatedConfigSource{}},
	}
}

// Resources 保存一份快照的 xDS 资源,顺序是确定的。
type Resources struct {
	Clusters  []types.Resource
	Endpoints []types.Resource
	Routes    []types.Resource
	Secrets   []types.Resource
}

// Translate 从 m 构建 CDS、EDS 和 RDS 资源。模型必须已经过校验且已排序
// (reconcile.Build 保证这两点)。
//
// 每个已声明的 entrypoint 都会得到一份 RouteConfiguration,即使没有路由,
// 这样通过 RDS 引用它的 Listener 不会一直等第一份响应。
// placeholders 里每个不是已声明 entrypoint 的名字会得到一份空的
// RouteConfiguration,于是 Envoy 有、但配置未声明的 listener 什么都不提供
// (404),而不是继续用过期路由。
// certificates 中的每一份证书生成一个同名的 SDS Secret。
func Translate(m *model.Model, placeholders []string, certificates []secrets.Certificate) Resources {
	var res Resources
	sorted := slices.Clone(certificates)
	slices.SortFunc(sorted, func(a, b secrets.Certificate) int { return strings.Compare(a.Name, b.Name) })
	for _, c := range sorted {
		res.Secrets = append(res.Secrets, &tlsv3.Secret{Name: c.Name, Type: &tlsv3.Secret_TlsCertificate{TlsCertificate: &tlsv3.TlsCertificate{
			CertificateChain: &core.DataSource{Specifier: &core.DataSource_InlineBytes{InlineBytes: slices.Clone(c.Certificate)}},
			PrivateKey:       &core.DataSource{Specifier: &core.DataSource_InlineBytes{InlineBytes: slices.Clone(c.PrivateKey)}},
		}}})
	}

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
						Timeout:          durationpb.New(r.Timeout),
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

// Version 是资源的内容哈希。用内容而不是时钟或计数器,意味着模型不变则版本不变,
// 重启后的控制平面会复现 Envoy 已经持有的版本。
func (r Resources) Version() (string, error) {
	h := sha256.New()
	opts := proto.MarshalOptions{Deterministic: true}
	for _, group := range [][]types.Resource{r.Clusters, r.Endpoints, r.Routes, r.Secrets} {
		for _, res := range group {
			b, err := opts.Marshal(res)
			if err != nil {
				return "", err
			}
			// 给每条消息加长度前缀,使拼接没有歧义。
			h.Write(binary.LittleEndian.AppendUint64(nil, uint64(len(b))))
			h.Write(b)
		}
		// 组分隔符。
		h.Write([]byte{0xff})
	}
	return hex.EncodeToString(h.Sum(nil))[:16], nil
}

// Snapshot 为这些资源构建一份一致的 cache 快照。
func (r Resources) Snapshot(version string) (*cache.Snapshot, error) {
	snap, err := cache.NewSnapshot(version, map[resource.Type][]types.Resource{
		resource.ClusterType:  r.Clusters,
		resource.EndpointType: r.Endpoints,
		resource.RouteType:    r.Routes,
		resource.SecretType:   r.Secrets,
	})
	if err != nil {
		return nil, err
	}
	if err := r.consistent(); err != nil {
		return nil, err
	}
	return snap, nil
}

// consistent 检查快照内部的交叉引用。上游的 Snapshot.Consistent 在这里不能用:
// 它要求每份 RouteConfiguration 都被某个 LDS listener 引用,但我们的 listener 是静态的。
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
