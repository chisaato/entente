package reconcile

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/chisaato/entente/internal/config"
	"github.com/chisaato/entente/internal/docker"
	"github.com/chisaato/entente/internal/model"
	"github.com/chisaato/entente/internal/secrets"
	"github.com/chisaato/entente/internal/xds"
)

// Material 表示已经读取到内存的材料,获取失败也作为输入显式传递。
type Material struct {
	Certificate secrets.Certificate
	Err         error
	Unchecked   bool
}
type PrepareOptions struct {
	Now          time.Time
	ColdStart    bool
	Runtime      bool
	Offline      bool
	Placeholders []string
}
type Report struct {
	Checked   []string
	Unchecked []string
	Warnings  []string
	Degraded  []string
	Rejected  []Rejection
	Errors    []error
}
type Prepared struct {
	Resources  xds.Resources
	Report     Report
	Generation string
}

// Prepare 只使用传入的内存视图和时间,不进行 I/O,不修改输入。
func Prepare(cfg *config.Config, containers []docker.Container, materials map[string]Material, options PrepareOptions) (*Prepared, error) {
	p := &Prepared{}
	if options.Now.IsZero() {
		p.Report.Errors = append(p.Report.Errors, fmt.Errorf("candidate preparation requires an explicit time"))
		return p, errors.Join(p.Report.Errors...)
	}
	m, rejected := Build(cfg, containers)
	declaredSecrets := map[string]bool{}
	for _, d := range cfg.Secrets {
		declaredSecrets[d.Name] = true
	}
	for _, l := range cfg.Listeners {
		if !slices.Contains(cfg.Entrypoints, l.Entrypoint) {
			p.Report.Errors = append(p.Report.Errors, fmt.Errorf("listener %q: undeclared entrypoint %q", l.Name, l.Entrypoint))
		}
		if l.Fallback.Secret != "" && !declaredSecrets[l.Fallback.Secret] {
			p.Report.Errors = append(p.Report.Errors, fmt.Errorf("listener %q: unknown fallback secret %q", l.Name, l.Fallback.Secret))
		}
		for _, b := range l.TLSBindings {
			if !declaredSecrets[b.Secret] {
				p.Report.Errors = append(p.Report.Errors, fmt.Errorf("listener %q binding %q: unknown secret %q", l.Name, b.Name, b.Secret))
			}
		}
	}
	p.Report.Rejected = rejected
	for _, r := range rejected {
		if r.Runtime {
			continue
		}
		if !options.Runtime || (options.ColdStart && strings.HasPrefix(r.Subject, "static/")) {
			p.Report.Errors = append(p.Report.Errors, fmt.Errorf("%s", r.String()))
		}
	}
	if containers == nil && cfg.Docker != nil {
		p.Report.Unchecked = append(p.Report.Unchecked, "docker snapshot")
	}
	var certs []secrets.Certificate
	for _, d := range cfg.Secrets {
		mat, ok := materials[d.Name]
		if mat.Unchecked && options.Offline {
			p.Report.Unchecked = append(p.Report.Unchecked, "secret/"+d.Name)
			continue
		}
		var leaf *x509.Certificate
		var err error
		if !ok || mat.Err != nil {
			err = fmt.Errorf("secret %q: certificate material unavailable", d.Name)
		} else {
			leaf, err = validateCertificate(mat.Certificate, options.Now)
		}
		if err == nil {
			for _, l := range cfg.Listeners {
				for _, b := range l.TLSBindings {
					if b.Secret != d.Name {
						continue
					}
					for _, h := range b.ServerNames {
						if !certificateCovers(leaf, h) {
							err = errors.Join(err, fmt.Errorf("secret %q: SAN does not cover %q", d.Name, h))
						}
					}
				}
			}
		}
		if err != nil {
			if (options.ColdStart || options.Runtime) && (!ok || mat.Err != nil) {
				p.Report.Degraded = append(p.Report.Degraded, err.Error())
				continue
			}
			p.Report.Errors = append(p.Report.Errors, fmt.Errorf("secret %q: %w", d.Name, err))
			continue
		}
		if leaf.NotAfter.Sub(options.Now) <= 7*24*time.Hour {
			p.Report.Warnings = append(p.Report.Warnings, fmt.Sprintf("secret %q expires within seven days", d.Name))
		}
		c := mat.Certificate
		c.Name = d.Name
		certs = append(certs, c)
		p.Report.Checked = append(p.Report.Checked, "secret/"+d.Name)
	}
	for _, l := range cfg.Listeners {
		if l.Fallback.StaticSecret != "" {
			p.Report.Unchecked = append(p.Report.Unchecked, "bootstrap secret/"+l.Fallback.StaticSecret)
		}
	}
	kept := m.Routes[:0]
	coverageRoutes := slices.Clone(m.Routes)
	// 后端暂不可用也不能掩盖 YAML 自身的 TLS 覆盖错误。
	for _, rejection := range rejected {
		if !rejection.Runtime {
			continue
		}
		for _, r := range cfg.Static.Routes {
			if "static/"+r.Name == rejection.Subject {
				coverageRoutes = append(coverageRoutes, model.Route{Name: rejection.Subject, Source: model.SourceStatic, Host: r.Host, Entrypoints: r.Entrypoints})
			}
		}
	}
	for _, r := range coverageRoutes {
		var problems []error
		for _, l := range cfg.Listeners {
			if !slices.Contains(r.Entrypoints, l.Entrypoint) {
				continue
			}
			covered := false
			for _, b := range l.TLSBindings {
				for _, h := range b.ServerNames {
					covered = covered || covers(h, r.Host)
				}
			}
			if !covered {
				problems = append(problems, fmt.Errorf("route %q: host %q is not covered by listener %q TLS bindings", r.Name, r.Host, l.Name))
			}
		}
		if len(problems) > 0 {
			if options.Runtime && r.Source == "docker" {
				p.Report.Rejected = append(p.Report.Rejected, Rejection{Subject: r.Name, Reason: errors.Join(problems...).Error()})
			} else {
				p.Report.Errors = append(p.Report.Errors, problems...)
			}
			continue
		}
		if r.Backend != "" {
			kept = append(kept, r)
		}
	}
	m.Routes = kept
	p.Resources = xds.Translate(m, options.Placeholders, certs)
	if err := p.Resources.Validate(); err != nil {
		p.Report.Errors = append(p.Report.Errors, err)
	}
	var err error
	p.Generation, err = p.Resources.Version()
	if err != nil {
		p.Report.Errors = append(p.Report.Errors, err)
	}
	return p, errors.Join(p.Report.Errors...)
}

func validateCertificate(c secrets.Certificate, now time.Time) (*x509.Certificate, error) {
	pair, err := tls.X509KeyPair(c.Certificate, c.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("invalid TLS certificate or mismatched private key")
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("invalid leaf certificate")
	}
	if now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) {
		return nil, fmt.Errorf("certificate is expired or not yet valid")
	}
	return leaf, nil
}

// covers 不允许具体域名覆盖通配声明,通配只匹配一个标签。
func covers(pattern, host string) bool {
	if pattern == host {
		return true
	}
	if strings.HasPrefix(host, "*.") {
		return false
	}
	if !strings.HasPrefix(pattern, "*.") {
		return false
	}
	suffix := pattern[1:]
	if !strings.HasSuffix(host, suffix) {
		return false
	}
	prefix := strings.TrimSuffix(host, suffix)
	return prefix != "" && !strings.Contains(prefix, ".")
}
func certificateCovers(c *x509.Certificate, host string) bool {
	for _, san := range c.DNSNames {
		if covers(strings.ToLower(san), host) {
			return true
		}
	}
	return false
}
