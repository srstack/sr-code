package embed

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
)

// Handler reverse-proxies to the child. It strips X-Frame-Options and
// Content-Security-Policy from responses (usher is same-site parent frame
// and its auth middleware is the perimeter), and forwards WebSocket upgrades.
func (p *Process) Handler() http.Handler {
	target, _ := url.Parse(p.ChildURL())
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ModifyResponse = func(r *http.Response) error {
		r.Header.Del("X-Frame-Options")
		r.Header.Del("Content-Security-Policy")
		return nil
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		http.Error(w, fmt.Sprintf("embed %q is not ready", p.spec.Name), http.StatusBadGateway)
	}
	return proxy
}
