package embed

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Handler reverse-proxies to the child. It strips X-Frame-Options and
// rewrites frame-ancestors out of Content-Security-Policy (usher is the
// same-site parent frame and its auth middleware is the perimeter) while
// preserving the child's remaining CSP directives — opencode web, for one,
// sends a full policy its UI is built under. WebSocket upgrades are
// forwarded.
func (p *Process) Handler() http.Handler {
	return p.proxy("")
}

// PathHandler serves the same child under an absolute URL prefix on usher's
// own origin (e.g. /embed/dsh/). The dedicated-port Handler only works when
// that port is reachable from the browser; behind a TLS reverse proxy it is
// not (and an https page cannot frame an http port), so the SPA embeds
// through this same-origin path. The child's root-absolute asset and API
// URLs are rebased onto the prefix: <base> plus attribute/url() rewriting in
// HTML/CSS, and an injected bootstrap that rebases fetch/XHR/WebSocket/
// EventSource and the history API at runtime. The bootstrap is a separate
// script (same-origin, so CSP 'self' allows it) rather than inline, which a
// strict child CSP would block.
func (p *Process) PathHandler(prefix string) http.Handler {
	prefix = strings.TrimSuffix(prefix, "/")
	inner := http.StripPrefix(prefix, p.proxy(prefix))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == prefix {
			http.Redirect(w, r, prefix+"/", http.StatusTemporaryRedirect)
			return
		}
		inner.ServeHTTP(w, r)
	})
}

// BootstrapPath is the same-origin script PathHandler injects; the web server
// serves its body (prefix baked in per embed).
func BootstrapPath(prefix string) string {
	return strings.TrimSuffix(prefix, "/") + "/__usher_bootstrap.js"
}

// BootstrapJS returns the runtime URL-rebasing script for one embed prefix.
func BootstrapJS(prefix string) []byte {
	lit, _ := json.Marshal(strings.TrimSuffix(prefix, "/"))
	return []byte(`(function(){var P=` + string(lit) + `;
function f(u){try{if(typeof u!=="string")return u;var q=new URL(u,location.href);
if(q.origin!==location.origin)return u;if(q.pathname===P||q.pathname.indexOf(P+"/")===0)return u;
return P+q.pathname+q.search+q.hash}catch(e){return u}}
var of=window.fetch;if(of)window.fetch=function(i,o){return of.call(this,typeof i==="string"?f(i):i,o)};
var xo=XMLHttpRequest.prototype.open;XMLHttpRequest.prototype.open=function(m,u){arguments[1]=f(u);return xo.apply(this,arguments)};
var W=window.WebSocket;if(W){var NW=function(u,p){return p===undefined?new W(f(u)):new W(f(u),p)};NW.prototype=W.prototype;window.WebSocket=NW}
var E=window.EventSource;if(E){var NE=function(u,c){return c===undefined?new E(f(u)):new E(f(u),c)};NE.prototype=E.prototype;window.EventSource=NE}
var ph=history.pushState,rh=history.replaceState;
history.pushState=function(s,t,u){if(u)u=f(String(u));return ph.apply(this,arguments)};
history.replaceState=function(s,t,u){if(u)u=f(String(u));return rh.apply(this,arguments)};
})();`)
}

// proxy builds the reverse proxy; a non-empty prefix enables body rebasing.
func (p *Process) proxy(rebase string) *httputil.ReverseProxy {
	target, _ := url.Parse(p.ChildURL())
	proxy := httputil.NewSingleHostReverseProxy(target)
	// Children fence browser requests by Host/Origin (dsh's trustedHosts
	// deputy defense). usher's auth middleware is the perimeter, and the
	// child only ever sees loopback traffic — so present the upstream
	// authority, exactly as a same-origin deployment would.
	director := proxy.Director
	proxy.Director = func(req *http.Request) {
		director(req)
		req.Host = target.Host
		if req.Header.Get("Origin") != "" {
			req.Header.Set("Origin", target.Scheme+"://"+target.Host)
		}
		if rebase != "" {
			// Force an identity upstream response: rebasing has to read the
			// body, and compressed bytes cannot be rewritten as text.
			req.Header.Del("Accept-Encoding")
		}
	}
	proxy.ModifyResponse = func(r *http.Response) error {
		r.Header.Del("X-Frame-Options")
		if csp := r.Header.Get("Content-Security-Policy"); csp != "" {
			var kept []string
			for _, dir := range strings.Split(csp, ";") {
				dir = strings.TrimSpace(dir)
				if dir == "" || strings.HasPrefix(strings.ToLower(dir), "frame-ancestors") {
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
		if rebase != "" {
			if err := rebaseResponse(r, rebase); err != nil {
				return err
			}
		}
		return nil
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		http.Error(w, fmt.Sprintf("embed %q is not ready", p.spec.Name), http.StatusBadGateway)
	}
	return proxy
}

// maxRebaseBytes caps body rewriting; larger payloads (bundles) pass through
// unchanged — the runtime bootstrap covers their API calls.
const maxRebaseBytes = 8 << 20

// rebaseResponse rewrites root-absolute URLs in HTML/CSS responses so the
// child's assets resolve under the proxy prefix, injects the base tag and
// runtime bootstrap into HTML, and rebases root-relative redirects.
func rebaseResponse(r *http.Response, prefix string) error {
	if loc := r.Header.Get("Location"); strings.HasPrefix(loc, "/") &&
		loc != prefix && !strings.HasPrefix(loc, prefix+"/") {
		r.Header.Set("Location", prefix+loc)
	}
	ct := r.Header.Get("Content-Type")
	isHTML := strings.Contains(ct, "text/html")
	isCSS := strings.Contains(ct, "text/css")
	if !isHTML && !isCSS {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRebaseBytes+1))
	if err != nil {
		return err
	}
	_ = r.Body.Close()
	if len(body) > maxRebaseBytes {
		r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), r.Body))
		return nil
	}
	if isHTML {
		body = rebaseHTML(body, prefix)
	} else {
		body = rebaseCSS(body, prefix)
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	r.Header.Set("Content-Length", strconv.Itoa(len(body)))
	r.Header.Del("Content-Encoding")
	return nil
}

var (
	htmlAttrRe = regexp.MustCompile(`(?i)\b(href|src|action|poster|data-src)="/([^/][^"]*)"`)
	htmlHeadRe = regexp.MustCompile(`(?i)<head[^>]*>`)
	cssURLRe   = regexp.MustCompile(`url\(\s*['"]?/([^)'"]*)['"]?\s*\)`)
)

// absURLRe remaps a root-absolute URL ("/foo") to the proxy prefix.
func rebaseRef(prefix, rest string) string {
	return prefix + "/" + rest
}

func rebaseHTML(body []byte, prefix string) []byte {
	out := htmlAttrRe.ReplaceAllFunc(body, func(m []byte) []byte {
		sub := htmlAttrRe.FindSubmatch(m)
		return []byte(string(sub[1]) + `="` + rebaseRef(prefix, string(sub[2])) + `"`)
	})
	inject := []byte(`<base href="` + prefix + `/"><script src="` + BootstrapPath(prefix) + `"></script>`)
	if loc := htmlHeadRe.FindIndex(out); loc != nil {
		merged := make([]byte, 0, len(out)+len(inject))
		merged = append(merged, out[:loc[1]]...)
		merged = append(merged, inject...)
		merged = append(merged, out[loc[1]:]...)
		return merged
	}
	return append(inject, out...)
}

func rebaseCSS(body []byte, prefix string) []byte {
	return cssURLRe.ReplaceAllFunc(body, func(m []byte) []byte {
		sub := cssURLRe.FindSubmatch(m)
		return []byte("url(" + rebaseRef(prefix, string(sub[1])) + ")")
	})
}
