package xds

import (
	"net/netip"
	"testing"
	"time"

	cluster "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	endpoint "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	route "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"

	"github.com/chisaato/entente/internal/model"
)

func testModel() *model.Model {
	return &model.Model{
		Entrypoints: []string{"http", "https"},
		Routes: []model.Route{
			{Name: "docker/whoami", Entrypoints: []string{"https"}, Host: "whoami.example.com", Backend: "docker/whoami"},
		},
		Backends: []model.Backend{
			{Name: "docker/whoami", Endpoints: []netip.AddrPort{netip.MustParseAddrPort("172.18.0.2:80")}},
		},
	}
}

func TestTranslateRouteTimeout(t *testing.T) {
	versions := map[string]bool{}
	for _, timeout := range []time.Duration{0, 60 * time.Second, 1500 * time.Millisecond} {
		m := testModel()
		m.Routes[0].Timeout = timeout
		res := Translate(m, nil, nil)
		rc := res.Routes[1].(*route.RouteConfiguration)
		action := rc.GetVirtualHosts()[0].GetRoutes()[0].GetRoute()
		if action.Timeout == nil || action.Timeout.AsDuration() != timeout {
			t.Fatalf("timeout = %v, want explicit %v", action.Timeout, timeout)
		}
		if err := action.Timeout.CheckValid(); err != nil {
			t.Fatal(err)
		}
		version, err := res.Version()
		if err != nil {
			t.Fatal(err)
		}
		if versions[version] {
			t.Error("timeout change did not change version")
		}
		versions[version] = true
	}
}

func TestTranslate(t *testing.T) {
	res := Translate(testModel(), nil, nil)

	if len(res.Clusters) != 1 || len(res.Endpoints) != 1 || len(res.Routes) != 2 {
		t.Fatalf("counts: clusters=%d endpoints=%d routes=%d", len(res.Clusters), len(res.Endpoints), len(res.Routes))
	}
	c := res.Clusters[0].(*cluster.Cluster)
	if c.GetType() != cluster.Cluster_EDS || c.GetEdsClusterConfig().GetServiceName() != "docker/whoami" {
		t.Errorf("cluster = %v", c)
	}
	if c.GetConnectTimeout().AsDuration() != upstreamConnectTimeout {
		t.Errorf("connect timeout not set explicitly")
	}
	cla := res.Endpoints[0].(*endpoint.ClusterLoadAssignment)
	sa := cla.GetEndpoints()[0].GetLbEndpoints()[0].GetEndpoint().GetAddress().GetSocketAddress()
	if sa.GetAddress() != "172.18.0.2" || sa.GetPortValue() != 80 {
		t.Errorf("endpoint = %v", sa)
	}

	httpRC := res.Routes[0].(*route.RouteConfiguration)
	if httpRC.GetName() != "http" || len(httpRC.GetVirtualHosts()) != 0 {
		t.Errorf("an entrypoint without routes must still get an empty RouteConfiguration: %v", httpRC)
	}
	httpsRC := res.Routes[1].(*route.RouteConfiguration)
	vh := httpsRC.GetVirtualHosts()[0]
	if vh.GetDomains()[0] != "whoami.example.com" || vh.GetRoutes()[0].GetRoute().GetCluster() != "docker/whoami" {
		t.Errorf("virtual host = %v", vh)
	}
}

func TestVersionIsContentHash(t *testing.T) {
	v1, err := Translate(testModel(), nil, nil).Version()
	if err != nil {
		t.Fatal(err)
	}
	v2, _ := Translate(testModel(), nil, nil).Version()
	if v1 != v2 {
		t.Errorf("same model, different versions: %s vs %s", v1, v2)
	}
	m := testModel()
	m.Backends[0].Endpoints[0] = netip.MustParseAddrPort("172.18.0.3:80")
	v3, _ := Translate(m, nil, nil).Version()
	if v3 == v1 {
		t.Error("endpoint change did not change the version")
	}
}

func TestSnapshotConsistent(t *testing.T) {
	res := Translate(testModel(), nil, nil)
	v, _ := res.Version()
	if _, err := res.Snapshot(v); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotRejectsDuplicateDomain(t *testing.T) {
	m := testModel()
	m.Routes = append(m.Routes, model.Route{Name: "docker/dup", Entrypoints: []string{"https"}, Host: "whoami.example.com", Backend: "docker/whoami"})
	if _, err := Translate(m, nil, nil).Snapshot("v"); err == nil {
		t.Fatal("duplicate domain in one RouteConfiguration must be caught before Envoy NACKs it")
	}
}

func TestTranslatePlaceholders(t *testing.T) {
	res := Translate(testModel(), []string{"https", "retired"}, nil)
	if len(res.Routes) != 3 {
		t.Fatalf("routes = %d, want http, https and the retired placeholder", len(res.Routes))
	}
	rc := res.Routes[2].(*route.RouteConfiguration)
	if rc.GetName() != "retired" || len(rc.GetVirtualHosts()) != 0 {
		t.Errorf("placeholder = %v", rc)
	}
}
