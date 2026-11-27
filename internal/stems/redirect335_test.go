package stems

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// #335 round 11 (item 2's class: every client that follows redirects with a
// credential): the Stems probe sends the backend's secret. On 5ea09903 it followed a
// redirect to another port of the same host with the secret (Go keeps Authorization
// there). Now it follows a redirect only within the backend's origin.
func TestOffersKeepsTheSecretOnTheBackend(t *testing.T) {
	var got atomic.Value
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Store(r.Header.Get("Authorization"))
		w.Write([]byte(`{"ok":true,"backend":"htdemucs"}`))
	}))
	defer other.Close()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/same/healthz/stems":
			http.Redirect(w, r, "/same/healthz/stems2", http.StatusFound)
		case "/same/healthz/stems2":
			if r.Header.Get("Authorization") != "Bearer stems-secret-r11" {
				http.Error(w, "no secret", http.StatusUnauthorized)
				return
			}
			w.Write([]byte(`{"ok":true,"backend":"htdemucs"}`))
		default:
			http.Redirect(w, r, other.URL+r.URL.Path, http.StatusFound)
		}
	}))
	defer backend.Close()
	_, err := Client{Endpoint: backend.URL, Secret: "stems-secret-r11"}.Offers(context.Background())
	if v, _ := got.Load().(string); v != "" {
		t.Fatalf("the probe took the secret to another origin: %q", v)
	}
	if err == nil {
		t.Fatal("the refused redirect reported no error")
	}
	// A redirect within the backend's origin is followed, with the secret.
	if backend, err := (Client{Endpoint: backend.URL + "/same", Secret: "stems-secret-r11"}).Offers(context.Background()); err != nil || backend != Neural {
		t.Fatalf("a redirect within the backend: %q %v", backend, err)
	}
}
