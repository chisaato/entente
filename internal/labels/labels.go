// Package labels 解析 Entente 的容器 label。
//
// Parse 是唯一知道如何用配置的前缀拼出 label key 的地方。
// 代码库的其他地方不得硬编码前缀或完整的 label key。
package labels

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// 相对于配置前缀的 label key。
const (
	keyEnable      = "enable"
	keyEntrypoints = "entrypoints"
	keyHost        = "host"
	keyPort        = "port"
	keyNetwork     = "network"
	keyPolicy      = "policy"
	keyTimeout     = "timeout"
)

var knownKeys = map[string]bool{
	keyEnable:      true,
	keyEntrypoints: true,
	keyHost:        true,
	keyPort:        true,
	keyNetwork:     true,
	keyPolicy:      true,
	keyTimeout:     true,
}

// Spec 是单个容器解析后的路由意图。
type Spec struct {
	Entrypoints []string
	Host        string
	Port        uint16
	// Timeout 为可选覆盖值;nil 表示继承顶层 route_timeout。
	Timeout *time.Duration
	// Network 是用于取容器 IP 的 Docker 网络。空表示"未指定";
	// 是否可接受由调用方决定。
	Network string
	// Policy 为预留字段。原样传出,以便 reconciler 用与静态路由相同的路径拒绝它。
	Policy string
}

// hostRe 接受小写 DNS 名,以及一个前导 "*." 通配。
var hostRe = regexp.MustCompile(`^(\*\.)?([a-z0-9]([a-z0-9-]*[a-z0-9])?\.)*[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// Parse 从容器 label 中提取 prefix 对应的 Spec。
//
// 容器没有 "<prefix>.enable=true" 时返回 enabled=false 且 err 为 nil;这类容器被忽略。
// 已启用的容器上,任何格式错误、缺失或未知的 "<prefix>.*" label 都是错误。
func Parse(prefix string, labels map[string]string) (spec Spec, enabled bool, err error) {
	own := map[string]string{}
	p := prefix + "."
	for k, v := range labels {
		if rest, ok := strings.CutPrefix(k, p); ok {
			own[rest] = v
		}
	}

	switch v, ok := own[keyEnable]; {
	case !ok || v == "false":
		return Spec{}, false, nil
	case v != "true":
		return Spec{}, false, fmt.Errorf("label %s%s=%q: want \"true\" or \"false\"", p, keyEnable, v)
	}

	var errs []string
	fail := func(format string, a ...any) { errs = append(errs, fmt.Sprintf(format, a...)) }

	var unknown []string
	for k := range own {
		if !knownKeys[k] {
			unknown = append(unknown, p+k)
		}
	}
	if len(unknown) > 0 {
		slices.Sort(unknown)
		fail("unknown labels %s", strings.Join(unknown, ", "))
	}

	if v, ok := own[keyEntrypoints]; !ok || strings.TrimSpace(v) == "" {
		fail("label %s%s is required (there is no default entrypoint)", p, keyEntrypoints)
	} else {
		seen := map[string]bool{}
		for _, ep := range strings.Split(v, ",") {
			ep = strings.TrimSpace(ep)
			switch {
			case ep == "":
				fail("label %s%s=%q: empty entrypoint name", p, keyEntrypoints, v)
			case seen[ep]:
				fail("label %s%s=%q: entrypoint %q listed twice", p, keyEntrypoints, v, ep)
			default:
				seen[ep] = true
				spec.Entrypoints = append(spec.Entrypoints, ep)
			}
		}
	}

	if v, ok := own[keyHost]; !ok || v == "" {
		fail("label %s%s is required", p, keyHost)
	} else if !hostRe.MatchString(v) {
		fail("label %s%s=%q: not a lower-case hostname", p, keyHost, v)
	} else {
		spec.Host = v
	}

	if v, ok := own[keyPort]; !ok || v == "" {
		fail("label %s%s is required", p, keyPort)
	} else if n, perr := strconv.ParseUint(v, 10, 16); perr != nil || n == 0 {
		fail("label %s%s=%q: not a port number", p, keyPort, v)
	} else {
		spec.Port = uint16(n)
	}

	spec.Network = own[keyNetwork]
	spec.Policy = own[keyPolicy]
	if v, ok := own[keyTimeout]; ok {
		if d, perr := time.ParseDuration(v); perr != nil || d < 0 {
			fail("label %s%s=%q: want a non-negative Go duration", p, keyTimeout, v)
		} else {
			spec.Timeout = &d
		}
	}

	if len(errs) > 0 {
		return Spec{}, true, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return spec, true, nil
}

// ValidHost 报告 h 能否作为路由 host。静态路由也用它,使两个来源共用同一定义。
func ValidHost(h string) bool { return hostRe.MatchString(h) }
