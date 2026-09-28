package xds

import (
	"slices"
	"sync"
)

// RouteNames records every route_config_name Envoy has asked for over RDS.
//
// The reconciler publishes an empty RouteConfiguration for requested names
// that are not declared entrypoints. Otherwise a listener whose entrypoint
// was removed from the config would keep its last routes (SotW RDS never
// deletes a RouteConfiguration), now pointing at deleted clusters, and a
// listener referencing a never-declared entrypoint would wait in warming.
type RouteNames struct {
	mu       sync.Mutex
	names    map[string]bool
	onChange func()
}

// NewRouteNames returns a tracker that calls onChange (if not nil) whenever a
// name is seen for the first time.
func NewRouteNames(onChange func()) *RouteNames {
	return &RouteNames{names: map[string]bool{}, onChange: onChange}
}

// Observe records requested names.
func (r *RouteNames) Observe(names []string) {
	r.mu.Lock()
	added := false
	for _, n := range names {
		if !r.names[n] {
			r.names[n] = true
			added = true
		}
	}
	onChange := r.onChange
	r.mu.Unlock()
	if added && onChange != nil {
		onChange()
	}
}

// SetOnChange replaces the change callback.
func (r *RouteNames) SetOnChange(f func()) {
	r.mu.Lock()
	r.onChange = f
	r.mu.Unlock()
}

// Names returns all names seen so far, sorted.
func (r *RouteNames) Names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.names))
	for n := range r.names {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}
