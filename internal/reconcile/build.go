// Package reconcile merges all sources into a validated model.Model.
package reconcile

import (
	"cmp"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"github.com/chisaato/entente/internal/config"
	"github.com/chisaato/entente/internal/docker"
	"github.com/chisaato/entente/internal/labels"
	"github.com/chisaato/entente/internal/model"
)

// Rejection records why a route (or container) was left out of the model.
type Rejection struct {
	// Subject is the route name, or "container/<name>" when no route could be
	// built from a container at all.
	Subject string
	Reason  string
}

func (r Rejection) String() string { return r.Subject + ": " + r.Reason }

// Build turns the config and the current Docker containers into a Model.
//
// Every check fails closed and is scoped to the offending route: a rejected
// route is left out and reported, all other routes are unaffected. When two
// or more routes claim the same host on the same entrypoint, all of them are
// rejected, because picking a winner would be an implicit rule.
func Build(cfg *config.Config, containers []docker.Container) (*model.Model, []Rejection) {
	var (
		routes   []model.Route
		backends = map[string]model.Backend{}
		rejected []Rejection
	)
	reject := func(subject, format string, a ...any) {
		rejected = append(rejected, Rejection{Subject: subject, Reason: fmt.Sprintf(format, a...)})
	}

	// Static source.
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
		})
	}

	// Docker source.
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
			})
		}
	}

	// Per-route checks.
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

	// Host conflicts per entrypoint.
	claims := map[[2]string][]string{} // {entrypoint, host} -> route names
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

// pickAddress returns the container's address on network. An empty network
// is only accepted when the container is attached to exactly one network.
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
