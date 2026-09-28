// Package labels parses Entente's container labels.
//
// Parse is the only place that knows how label keys are built from the
// configured prefix. Nothing else in the code base may hard-code a prefix or
// a full label key.
package labels

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Label keys, relative to the configured prefix.
const (
	keyEnable      = "enable"
	keyEntrypoints = "entrypoints"
	keyHost        = "host"
	keyPort        = "port"
	keyNetwork     = "network"
	keyPolicy      = "policy"
)

var knownKeys = map[string]bool{
	keyEnable:      true,
	keyEntrypoints: true,
	keyHost:        true,
	keyPort:        true,
	keyNetwork:     true,
	keyPolicy:      true,
}

// Spec is the parsed routing intent of one container.
type Spec struct {
	Entrypoints []string
	Host        string
	Port        uint16
	// Network is the Docker network to take the container IP from. Empty
	// means "not specified"; the caller decides whether that is acceptable.
	Network string
	// Policy is reserved. It is passed through so the reconciler can reject
	// it on the same path as static routes.
	Policy string
}

// hostRe accepts lower-case DNS names and a single leading "*." wildcard.
var hostRe = regexp.MustCompile(`^(\*\.)?([a-z0-9]([a-z0-9-]*[a-z0-9])?\.)*[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// Parse extracts the Spec for prefix from a container's labels.
//
// It returns enabled=false with a nil error when the container carries no
// "<prefix>.enable=true" label; such containers are ignored. Any malformed,
// missing or unknown "<prefix>.*" label on an enabled container is an error.
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

	if len(errs) > 0 {
		return Spec{}, true, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return spec, true, nil
}

// ValidHost reports whether h is acceptable as a route host. Static routes use
// it so both sources share one definition.
func ValidHost(h string) bool { return hostRe.MatchString(h) }
