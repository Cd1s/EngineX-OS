package webui

import (
	"net/http/httptest"
	"testing"
)

// #218: the console QR code pointed at sc5000.lan (enginex.lan since 5.3.0.5, #234), which most
// routers do not resolve, so phones opened a blank page.
func TestConsoleQRPointsWherePhonesCanReach(t *testing.T) {
	lan := "http://" + lanHost() + ":8080"
	for host, want := range map[string]string{
		"192.168.5.34:8080":         "http://192.168.5.34:8080",
		"enginex.local:8080":        "http://enginex.local:8080",
		"127.0.0.1:8080":            lan,
		"localhost:8080":            lan,
		"enginex.lan:8080":          lan,
		"sc5000.lan:8080":           lan,
		"[::1]:8080":                lan,
		"abc-def.trycloudflare.com": "https://abc-def.trycloudflare.com",
	} {
		r := httptest.NewRequest("GET", "/api/console-qr", nil)
		r.Host = host
		if got := consoleURL(r); got != want {
			t.Errorf("Host %s: %s, want %s", host, got, want)
		}
	}
	r := httptest.NewRequest("GET", "/api/console-qr", nil)
	r.Host = "192.168.5.34:8080"
	for _, def := range []string{"http://enginex.lan:8080", "http://sc5000.lan:8080"} { // a saved one from before 5.3.0.5 too
		if got := (&Server{cfg: Config{PublicURL: def}}).qrTarget(r); got != "http://192.168.5.34:8080" {
			t.Errorf("default public URL %s: %s", def, got)
		}
	}
	if got := (&Server{cfg: Config{PublicURL: "https://deck.example.org"}}).qrTarget(r); got != "https://deck.example.org" {
		t.Errorf("configured public URL: %s", got)
	}
	if hintTitle("netease") == "" || lanHost() == "" {
		t.Fatal("empty hint")
	}
}
