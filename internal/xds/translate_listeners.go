package xds

import (
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"github.com/chisaato/entente/internal/config"
	"github.com/chisaato/entente/internal/secrets"
	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listener "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	route "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	router "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/router/v3"
	inspector "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/listener/tls_inspector/v3"
	hcm "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	tls "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

func packed(m proto.Message) *anypb.Any {
	a, err := anypb.New(m)
	if err != nil {
		panic(err)
	}
	return a
}

func translateListeners(definitions []config.Listener, certificates []secrets.Certificate) []types.Resource {
	available := map[string]bool{}
	for _, c := range certificates {
		available[c.Name] = true
	}
	definitions = slices.Clone(definitions)
	slices.SortFunc(definitions, func(a, b config.Listener) int { return strings.Compare(a.Name, b.Name) })
	var out []types.Resource
	for _, d := range definitions {
		// 兜底材料缺失时暂不发布整个 listener,不能制造缺 SDS 引用。
		if d.Fallback.Secret != "" && !available[d.Fallback.Secret] {
			continue
		}
		ap, err := netip.ParseAddrPort(d.Address)
		if err != nil {
			continue
		}
		l := &listener.Listener{Name: d.Name, Address: &core.Address{Address: &core.Address_SocketAddress{SocketAddress: &core.SocketAddress{Address: ap.Addr().String(), PortSpecifier: &core.SocketAddress_PortValue{PortValue: uint32(ap.Port())}}}}, ListenerFilters: []*listener.ListenerFilter{{Name: "envoy.filters.listener.tls_inspector", ConfigType: &listener.ListenerFilter_TypedConfig{TypedConfig: packed(&inspector.TlsInspector{})}}}}
		bindings := slices.Clone(d.TLSBindings)
		slices.SortFunc(bindings, func(a, b config.TLSBinding) int { return strings.Compare(a.Name, b.Name) })
		for _, b := range bindings {
			if !available[b.Secret] {
				continue
			}
			names := slices.Clone(b.ServerNames)
			slices.Sort(names)
			chain := listenerChain(d, b.Name, b.Secret, false, false)
			chain.FilterChainMatch = &listener.FilterChainMatch{ServerNames: names}
			l.FilterChains = append(l.FilterChains, chain)
		}
		name := d.Fallback.Secret
		static := false
		if name == "" {
			name = d.Fallback.StaticSecret
			static = true
		}
		l.DefaultFilterChain = listenerChain(d, d.Name+"/sink", name, static, true)
		out = append(out, l)
	}
	return out
}

func listenerChain(d config.Listener, name, secret string, static, sink bool) *listener.FilterChain {
	sds := &tls.SdsSecretConfig{Name: secret}
	if !static {
		sds.SdsConfig = adsSource()
	}
	ctx := &tls.DownstreamTlsContext{CommonTlsContext: &tls.CommonTlsContext{AlpnProtocols: slices.Clone(d.ALPN), TlsCertificateSdsSecretConfigs: []*tls.SdsSecretConfig{sds}}}
	h := &hcm.HttpConnectionManager{StatPrefix: d.Name, HttpFilters: []*hcm.HttpFilter{{Name: "envoy.filters.http.router", ConfigType: &hcm.HttpFilter_TypedConfig{TypedConfig: packed(&router.Router{})}}}}
	if d.HCM.StripAnyHostPort != nil {
		h.StripPortMode = &hcm.HttpConnectionManager_StripAnyHostPort{StripAnyHostPort: *d.HCM.StripAnyHostPort}
	}
	if d.HCM.Websocket != nil {
		h.UpgradeConfigs = []*hcm.HttpConnectionManager_UpgradeConfig{{UpgradeType: "websocket", Enabled: wrapperspb.Bool(*d.HCM.Websocket)}}
	}
	if sink {
		h.RouteSpecifier = &hcm.HttpConnectionManager_RouteConfig{RouteConfig: &route.RouteConfiguration{Name: d.Name + "/sink", VirtualHosts: []*route.VirtualHost{{Name: "sink", Domains: []string{"*"}, Routes: []*route.Route{{Match: &route.RouteMatch{PathSpecifier: &route.RouteMatch_Prefix{Prefix: "/"}}, Action: &route.Route_DirectResponse{DirectResponse: &route.DirectResponseAction{Status: 421}}}}}}}}
	} else {
		h.RouteSpecifier = &hcm.HttpConnectionManager_Rds{Rds: &hcm.Rds{ConfigSource: adsSource(), RouteConfigName: d.Entrypoint}}
	}
	return &listener.FilterChain{Name: name, TransportSocket: &core.TransportSocket{Name: "envoy.transport_sockets.tls", ConfigType: &core.TransportSocket_TypedConfig{TypedConfig: packed(ctx)}}, Filters: []*listener.Filter{{Name: "envoy.filters.network.http_connection_manager", ConfigType: &listener.Filter_TypedConfig{TypedConfig: packed(h)}}}}
}

func (r Resources) validateListeners() error {
	known := map[string]bool{}
	for _, s := range r.Secrets {
		known[s.(*tls.Secret).Name] = true
	}
	routes := map[string]bool{}
	for _, rt := range r.Routes {
		routes[rt.(*route.RouteConfiguration).Name] = true
	}
	for _, raw := range r.Listeners {
		l := raw.(*listener.Listener)
		chains := append(slices.Clone(l.FilterChains), l.DefaultFilterChain)
		for _, c := range chains {
			if c == nil {
				continue
			}
			ctx := &tls.DownstreamTlsContext{}
			if err := c.GetTransportSocket().GetTypedConfig().UnmarshalTo(ctx); err != nil {
				return err
			}
			if err := ctx.ValidateAll(); err != nil {
				return err
			}
			for _, s := range ctx.GetCommonTlsContext().GetTlsCertificateSdsSecretConfigs() {
				if s.SdsConfig != nil && !known[s.Name] {
					return fmt.Errorf("listener %q references missing secret %q", l.Name, s.Name)
				}
			}
			for _, f := range c.Filters {
				h := &hcm.HttpConnectionManager{}
				if err := f.GetTypedConfig().UnmarshalTo(h); err != nil {
					return err
				}
				if err := h.ValidateAll(); err != nil {
					return err
				}
				if h.GetRds() != nil && !routes[h.GetRds().RouteConfigName] {
					return fmt.Errorf("listener %q references missing route config", l.Name)
				}
			}
		}
	}
	return nil
}

// Validate 对所有生成的资源执行 proto 校验及跨资源一致性校验。
func (r Resources) Validate() error {
	var errs []error
	for _, group := range [][]types.Resource{r.Clusters, r.Endpoints, r.Routes, r.Secrets, r.Listeners} {
		for _, res := range group {
			if v, ok := res.(interface{ ValidateAll() error }); ok {
				if err := v.ValidateAll(); err != nil {
					errs = append(errs, err)
				}
			}
		}
	}
	if err := r.consistent(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
