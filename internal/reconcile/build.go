// Package reconcile 把所有来源合并成经过校验的 model.Model。
package reconcile

import (
	"cmp"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/chisaato/entente/internal/config"
	"github.com/chisaato/entente/internal/docker"
	"github.com/chisaato/entente/internal/labels"
	"github.com/chisaato/entente/internal/model"
)

// Rejection 记录一条路由(或一个容器)被排除出模型的原因。
type Rejection struct {
	// Subject 是路由名;如果根本无法从容器构建出路由,则为 "container/<name>"。
	Subject string
	Reason  string
}

func (r Rejection) String() string { return r.Subject + ": " + r.Reason }

// Build 把配置和当前的 Docker 容器变成 Model。
//
// 每项检查都 fail closed,并且只作用于出错的那条路由:被拒绝的路由被排除并上报,
// 其他路由不受影响。两条及以上路由在同一 entrypoint 上声明同一 host 时,全部拒绝,
// 因为挑出一个赢家本身就是隐含规则。
func Build(cfg *config.Config, containers []docker.Container) (*model.Model, []Rejection) {
	var (
		routes   []model.Route
		backends = map[string]model.Backend{}
		rejected []Rejection
	)
	reject := func(subject, format string, a ...any) {
		rejected = append(rejected, Rejection{Subject: subject, Reason: fmt.Sprintf(format, a...)})
	}

	// 静态来源。
	for _, b := range cfg.Static.Backends {
		name := "static/" + b.Name
		backends[name] = model.Backend{Name: name, Source: model.SourceStatic, Endpoints: b.ParsedEndpoints()}
	}
	for _, r := range cfg.Static.Routes {
		routes = append(routes, model.Route{
			Name:        "static/" + r.Name,
			Source:      model.SourceStatic,
			Entrypoints: r.Entrypoints,
			Host:        r.Host,
			Backend:     "static/" + r.Backend,
			PolicyRef:   r.Policy,
			Timeout:     routeTimeout(cfg, r.ParsedTimeout()),
		})
	}

	// Docker 来源。
	if cfg.Docker != nil {
		for _, c := range containers {
			spec, enabled, err := labels.Parse(cfg.Docker.LabelPrefix, c.Labels)
			if !enabled && err == nil {
				continue
			}
			subject := "container/" + c.Name
			if err != nil {
				reject(subject, "%v", err)
				continue
			}
			addr, err := pickAddress(c, spec.Network)
			if err != nil {
				reject(subject, "%v", err)
				continue
			}
			name := "docker/" + c.Name
			backends[name] = model.Backend{
				Name:      name,
				Source:    model.SourceDocker,
				Endpoints: []netip.AddrPort{netip.AddrPortFrom(addr, spec.Port)},
			}
			routes = append(routes, model.Route{
				Name:        name,
				Source:      model.SourceDocker,
				Entrypoints: spec.Entrypoints,
				Host:        spec.Host,
				Backend:     name,
				PolicyRef:   spec.Policy,
				Timeout:     routeTimeout(cfg, spec.Timeout),
			})
		}
	}

	// 逐条路由检查。
	declared := map[string]bool{}
	for _, ep := range cfg.Entrypoints {
		declared[ep] = true
	}
	var valid []model.Route
	for _, r := range routes {
		if problem := checkRoute(r, declared, backends); problem != "" {
			reject(r.Name, "%s", problem)
			continue
		}
		valid = append(valid, r)
	}

	// 每个 entrypoint 上的 host 冲突:{entrypoint, host} -> 路由名。
	claims := map[[2]string][]string{}
	for _, r := range valid {
		for _, ep := range r.Entrypoints {
			k := [2]string{ep, r.Host}
			claims[k] = append(claims[k], r.Name)
		}
	}
	conflicted := map[string][]string{}
	for k, names := range claims {
		if len(names) < 2 {
			continue
		}
		for _, n := range names {
			conflicted[n] = append(conflicted[n], fmt.Sprintf("host %q on entrypoint %q is also claimed by %s",
				k[1], k[0], strings.Join(without(names, n), ", ")))
		}
	}

	m := &model.Model{Entrypoints: slices.Clone(cfg.Entrypoints)}
	used := map[string]bool{}
	for _, r := range valid {
		if reasons, ok := conflicted[r.Name]; ok {
			slices.Sort(reasons)
			reject(r.Name, "%s", strings.Join(reasons, "; "))
			continue
		}
		m.Routes = append(m.Routes, r)
		used[r.Backend] = true
	}
	for name := range used {
		m.Backends = append(m.Backends, backends[name])
	}

	slices.Sort(m.Entrypoints)
	slices.SortFunc(m.Routes, func(a, b model.Route) int { return cmp.Compare(a.Name, b.Name) })
	slices.SortFunc(m.Backends, func(a, b model.Backend) int { return cmp.Compare(a.Name, b.Name) })
	slices.SortFunc(rejected, func(a, b Rejection) int { return cmp.Compare(a.Subject, b.Subject) })
	return m, rejected
}

// routeTimeout 只在未提供覆盖值时继承顶层配置,显式 0 不会被当成缺省。
func routeTimeout(cfg *config.Config, override *time.Duration) time.Duration {
	if override != nil {
		return *override
	}
	return cfg.ParsedRouteTimeout()
}

func checkRoute(r model.Route, declared map[string]bool, backends map[string]model.Backend) string {
	if r.PolicyRef != "" {
		return fmt.Sprintf("policy %q requested, but the policy layer is not implemented yet", r.PolicyRef)
	}
	if len(r.Entrypoints) == 0 {
		return "no entrypoints (there is no default entrypoint)"
	}
	for _, ep := range r.Entrypoints {
		if !declared[ep] {
			return fmt.Sprintf("entrypoint %q is not declared in the config", ep)
		}
	}
	if !labels.ValidHost(r.Host) {
		return fmt.Sprintf("host %q is not a lower-case hostname", r.Host)
	}
	if _, ok := backends[r.Backend]; !ok {
		return fmt.Sprintf("backend %q does not exist", r.Backend)
	}
	return ""
}

// pickAddress 返回容器在 network 上的地址。network 为空时,
// 仅当容器只挂在恰好一张网络上才接受。
func pickAddress(c docker.Container, network string) (netip.Addr, error) {
	if network == "" {
		if len(c.Networks) != 1 {
			names := make([]string, 0, len(c.Networks))
			for n := range c.Networks {
				names = append(names, n)
			}
			slices.Sort(names)
			return netip.Addr{}, fmt.Errorf("attached to %d networks %v; set the network label to choose one", len(names), names)
		}
		for n := range c.Networks {
			network = n
		}
	}
	addr, ok := c.Networks[network]
	if !ok {
		return netip.Addr{}, fmt.Errorf("not attached to network %q", network)
	}
	if !addr.IsValid() {
		return netip.Addr{}, fmt.Errorf("no IP address on network %q", network)
	}
	return addr, nil
}

func without(names []string, drop string) []string {
	out := make([]string, 0, len(names)-1)
	for _, n := range names {
		if n != drop {
			out = append(out, n)
		}
	}
	return out
}
