package webui

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
)

// serviceIcons are the console's and phone pages' service marks (#67). On a deck
// the UI pack's official PNG (icons/<id>.png) wins; these simplified marks cover
// the services whose artwork only Engine itself ships (SoundCloud, Beatport,
// TIDAL) and a deck without the UI pack.
var serviceIcons = map[string]string{
	"netease":    `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64"><rect width="64" height="64" rx="14" fill="#e60026"/><path d="M36 14c-9 1-16 8-16 17 0 8 6 14 13 14 6 0 11-5 11-11 0-5-4-9-9-9-4 0-7 3-7 7 0 3 2 5 5 5" fill="none" stroke="#fff" stroke-width="4.5" stroke-linecap="round"/><path d="M34 18l2 13" stroke="#fff" stroke-width="4.5" stroke-linecap="round"/><path d="M41 16c4 1 7 4 8 8" fill="none" stroke="#fff" stroke-width="4" stroke-linecap="round"/></svg>`,
	"spotify":    `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64"><circle cx="32" cy="32" r="30" fill="#1ed760"/><g fill="none" stroke="#000" stroke-linecap="round"><path d="M15 24c11-4 24-3 34 3" stroke-width="5.5"/><path d="M17 33c9-3 20-2 28 3" stroke-width="4.5"/><path d="M19 41c7-2 16-2 22 2" stroke-width="3.8"/></g></svg>`,
	"applemusic": `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64"><defs><linearGradient id="g" x1="0" y1="0" x2="0" y2="1"><stop offset="0" stop-color="#fc5c7d"/><stop offset="1" stop-color="#fa233b"/></linearGradient></defs><rect width="64" height="64" rx="14" fill="url(#g)"/><path d="M26 17l20-4v26" fill="none" stroke="#fff" stroke-width="3.2" stroke-linejoin="round"/><path d="M26 17v26" stroke="#fff" stroke-width="3.2"/><path d="M26 17l20-4v5l-20 4z" fill="#fff"/><ellipse cx="21" cy="43" rx="6" ry="4.6" fill="#fff"/><ellipse cx="41" cy="39" rx="6" ry="4.6" fill="#fff"/></svg>`,
	"soundcloud": `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64"><rect width="64" height="64" rx="14" fill="#ff5500"/><g fill="#fff"><rect x="8" y="33" width="2.6" height="9" rx="1.3"/><rect x="13" y="29" width="2.6" height="13" rx="1.3"/><rect x="18" y="26" width="2.6" height="16" rx="1.3"/><rect x="23" y="24" width="2.6" height="18" rx="1.3"/><path d="M28 42V24.5c1.6-1 3.6-1.5 5.6-1.5 6 0 10.9 4.6 11.4 10.4.8-.3 1.7-.4 2.6-.4 4.6 0 8.4 4 8.4 9z"/></g></svg>`,
	"deezer":     `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64"><rect width="64" height="64" rx="14" fill="#a238ff"/><g fill="#fff"><rect x="44" y="18" width="10" height="5" rx="1"/><rect x="44" y="25" width="10" height="5" rx="1"/><rect x="31" y="25" width="10" height="5" rx="1"/><rect x="44" y="32" width="10" height="5" rx="1"/><rect x="31" y="32" width="10" height="5" rx="1"/><rect x="18" y="32" width="10" height="5" rx="1"/><rect x="44" y="39" width="10" height="5" rx="1"/><rect x="31" y="39" width="10" height="5" rx="1"/><rect x="18" y="39" width="10" height="5" rx="1"/><rect x="5" y="39" width="10" height="5" rx="1"/></g></svg>`,
	"beatport":   `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64"><rect width="64" height="64" rx="14" fill="#000"/><g fill="none" stroke="#01ff95" stroke-width="6" stroke-linecap="round"><path d="M22 12v23"/><circle cx="33" cy="38" r="11"/></g></svg>`,
	"tidal":      `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64"><rect width="64" height="64" rx="14" fill="#000"/><g fill="#fff"><path d="M16 18l7 7-7 7-7-7z"/><path d="M32 18l7 7-7 7-7-7z"/><path d="M48 18l7 7-7 7-7-7z"/><path d="M32 34l7 7-7 7-7-7z"/></g></svg>`,
}

// serviceIcon is public: phone pages have no console cookie, and only the ids
// above are served, so nothing else on disk can be reached.
func (s *Server) serviceIcon(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	svg, ok := serviceIcons[id]
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "max-age=86400")
	if b, err := os.ReadFile(filepath.Join(s.uiDir(), "icons", id+".png")); err == nil {
		w.Header().Set("Content-Type", "image/png")
		w.Write(b)
		return
	}
	w.Header().Set("Content-Type", "image/svg+xml")
	io.WriteString(w, svg)
}
