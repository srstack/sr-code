package embed

import (
	"log/slog"
	"net/http/httptest"
)

// NewForTest returns a ready Process that proxies to an httptest upstream,
// so other packages (e.g. internal/web) can exercise their embed wiring
// without spawning a child process.
func NewForTest(upstream *httptest.Server) *Process {
	p := &Process{
		spec:     Spec{Name: "test", Title: "Test"},
		childURL: upstream.URL,
		logger:   slog.Default(),
	}
	p.ready.Store(true)
	return p
}
