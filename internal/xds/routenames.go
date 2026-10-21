package xds

import (
	"slices"
	"sync"
)

// RouteNames 记录 Envoy 通过 RDS 请求过的每一个 route_config_name。
//
// reconciler 会给那些不是已声明 entrypoint 的请求名发布一份空的 RouteConfiguration。
// 否则,entrypoint 已从配置中删除的 listener 会保留最后的路由
// (SotW RDS 从不删除 RouteConfiguration),而这些路由指向已删除的 cluster;
// 引用从未声明的 entrypoint 的 listener 则会停在 warming。
type RouteNames struct {
	mu       sync.Mutex
	names    map[string]bool
	onChange func()
}

// NewRouteNames 返回一个追踪器,每当某个名字第一次出现时调用 onChange(若不为 nil)。
func NewRouteNames(onChange func()) *RouteNames {
	return &RouteNames{names: map[string]bool{}, onChange: onChange}
}

// Observe 记录请求到的名字。
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

// SetOnChange 替换变化回调。
func (r *RouteNames) SetOnChange(f func()) {
	r.mu.Lock()
	r.onChange = f
	r.mu.Unlock()
}

// Names 返回迄今见过的全部名字,已排序。
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
