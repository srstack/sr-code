package embed

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// Handler reverse-proxies to the child. It strips X-Frame-Options and
// rewrites frame-ancestors out of Content-Security-Policy (usher is the
// same-site parent frame and its auth middleware is the perimeter) while
// preserving the child's remaining CSP directives — opencode web, for one,
// sends a full policy its UI is built under. WebSocket upgrades are
// forwarded.
func (p *Process) Handler() http.Handler {
	target, _ := url.Parse(p.ChildURL())
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ModifyResponse = func(r *http.Response) error {
		r.Header.Del("X-Frame-Options")
		if csp := r.Header.Get("Content-Security-Policy"); csp != "" {
			var kept []string
			for _, dir := range strings.Split(csp, ";") {
				dir = strings.TrimSpace(dir)
				if dir == "" || strings.HasPrefix(dir, "frame-ancestors") {
					continue
				}
				kept = append(kept, dir)
			}
			if len(kept) == 0 {
				r.Header.Del("Content-Security-Policy")
			} else {
				r.Header.Set("Content-Security-Policy", strings.Join(kept, "; "))
			}
		}
		return nil
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		http.Error(w, fmt.Sprintf("embed %q is not ready", p.spec.Name), http.StatusBadGateway)
	}
	return proxy
}
