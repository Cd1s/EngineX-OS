package providers

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAppleMusicScraperHandlesRotatedBundleFilenames(t *testing.T) {
	token := fakeJWT(time.Now().Add(30 * 24 * time.Hour))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/us/browse":
			// Apple rotated away from /assets/index*.js to chunked entry/vendor scripts.
			io.WriteString(w, `<!doctype html><html><head>`+
				`<script src="https://evil.example/steal.js"></script>`+
				`<script type="module" crossorigin src="/assets/runtime-77a91c.js"></script>`+
				`<script type="module" crossorigin src="/assets/musickit-web-9b8f12.js"></script>`+
				`</head></html>`)
		case "/assets/runtime-77a91c.js":
			io.WriteString(w, `export const chunk="runtime";`)
		case "/assets/musickit-web-9b8f12.js":
			fmt.Fprintf(w, `const cfg={developerToken:%q};export default cfg;`, token)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	am := &AppleMusic{BaseURL: srv.URL, WebURL: srv.URL}
	got, err := am.DeveloperToken(context.Background())
	if err != nil || got != token {
		t.Fatalf("DeveloperToken on rotated bundle filename = %q, err = %v; want %q", got, err, token)
	}
}
