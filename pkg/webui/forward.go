package webui

import (
	"net/http"
	"path"
)

// ForwardAddr is the console listener that the Quick Tunnel and the Tailscale console
// forward reach. Their callers come from outside the deck through a local process, so they
// arrive from loopback like Engine does; this listener serves them the console and nothing
// that trusts a loopback caller.
const ForwardAddr = "127.0.0.1:8081"

// ForwardHandler is the handler for ForwardAddr: the console, behind its login, without the
// deck-local streaming gateway (/v1/, /au/).
func (s *Server) ForwardHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isTidal(r.URL.Path) || isTidal(path.Clean("/"+r.URL.Path)+"/") {
			w.Header().Set("Cache-Control", "no-store")
			http.NotFound(w, r)
			return
		}
		s.ServeHTTP(w, r)
	})
}
