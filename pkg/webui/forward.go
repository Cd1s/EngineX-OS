package webui

import (
	"net/http"
	"path"
	"strings"
)

// ForwardAddr is the console listener that the Quick Tunnel and the Tailscale console
// forward reach. Their callers come from outside the deck through a local process, so they
// arrive from loopback like Engine does; this listener serves them the console and nothing
// that trusts a loopback caller.
const ForwardAddr = "127.0.0.1:8081"

// lanOnly reports a route that units on the LAN call with no login: a linked player reads this
// deck's state and the loaded track (/peer/), Spotify's app signs the deck in (/spotify-connect).
func lanOnly(p string) bool {
	clean := path.Clean("/"+p) + "/"
	return strings.HasPrefix(clean, "/peer/") || strings.HasPrefix(clean, "/spotify-connect/")
}

// ForwardHandler is the handler for ForwardAddr: the console, behind its login, without the
// deck-local streaming gateway (/v1/, /au/) and without the routes for units on the LAN.
func (s *Server) ForwardHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isTidal(r.URL.Path) || isTidal(path.Clean("/"+r.URL.Path)+"/") || lanOnly(r.URL.Path) {
			w.Header().Set("Cache-Control", "no-store")
			http.NotFound(w, r)
			return
		}
		s.ServeHTTP(w, r)
	})
}
