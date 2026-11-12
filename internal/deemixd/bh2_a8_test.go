package deemixd

import (
	"context"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrivateLANRelayAcceptedInCredentials(t *testing.T) {
	d := testDaemon(t)
	arl := strings.Repeat("d", 192)
	d.hc = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "192.168.1.50:6595" || r.URL.Path != "/api/config" || r.Header.Get("X-Deemix-Password") != "lan-pass" {
			t.Fatalf("unexpected relay request: %s %v", r.URL.String(), r.Header)
		}
		body := `{"config":{"deezerArl":"` + arl + `","deezerUserId":"42"}}`
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	writeState(t, filepath.Join(d.stateDir, "deemix-config.json"), `{"relayUrl":"http://192.168.1.50:6595","deemixPassword":"lan-pass"}`)

	got, err := d.credentials(context.Background())
	if err != nil || got != arl {
		t.Fatalf("private LAN relay rejected by deemixd credentials: got=%q err=%v", got, err)
	}
}
