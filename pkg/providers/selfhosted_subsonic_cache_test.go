package providers_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/deemixclient"
	"github.com/Cd1s/EngineX-OS/pkg/providers"
)

// hunt3SubsonicServer answers every request with a small body that names the asked ID.
func hunt3SubsonicServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write([]byte("SUBSONIC-AUDIO-" + r.URL.Query().Get("id")))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func hunt3Subsonic(srv *httptest.Server) *providers.SelfHostedProvider {
	return providers.NewSelfHostedProvider(providers.NASConfig{Protocol: "subsonic", URL: srv.URL, Username: "user", Password: "pass", Enabled: true}, srv.Client())
}

// Bug hunt 3: Subsonic downloads were named subsonic-<id>, a name the cache budget never evicts.
func TestSubsonicDownloadIsEvictedByTheCacheBudget(t *testing.T) {
	p := hunt3Subsonic(hunt3SubsonicServer(t))
	dir := t.TempDir()
	first, err := p.DownloadTrack(context.Background(), "101", dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := p.DownloadTrack(context.Background(), "102", dir)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := regexp.MatchString(`^nas-[0-9a-f]{64}\.mp3$`, filepath.Base(first)); !ok {
		t.Fatalf("a Subsonic download is named %q, want the shared nas-<hash> name", filepath.Base(first))
	}
	c := &deemixclient.Client{DownloadDir: dir, MaxCache: 1}
	c.Trim(second)
	if _, err := os.Stat(first); !os.IsNotExist(err) {
		t.Fatalf("the older Subsonic file stays outside the cache budget: %v", err)
	}
	if _, err := os.Stat(second); err != nil {
		t.Fatalf("the file being kept was removed: %v", err)
	}
}

// Two IDs that differ only in characters the old name dropped shared one file, and so did
// the same ID on two servers.
func TestSubsonicIDsNeverShareACacheFile(t *testing.T) {
	p := hunt3Subsonic(hunt3SubsonicServer(t))
	dir := t.TempDir()
	a, err := p.DownloadTrack(context.Background(), "A:1", dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := p.DownloadTrack(context.Background(), "A/1", dir)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatalf("the IDs A:1 and A/1 share the file %s", filepath.Base(a))
	}
	gotA, _ := os.ReadFile(a)
	gotB, _ := os.ReadFile(b)
	if string(gotA) != "SUBSONIC-AUDIO-A:1" || string(gotB) != "SUBSONIC-AUDIO-A/1" {
		t.Fatalf("contents %q and %q", gotA, gotB)
	}
	other, err := hunt3Subsonic(hunt3SubsonicServer(t)).DownloadTrack(context.Background(), "A:1", dir)
	if err != nil {
		t.Fatal(err)
	}
	if other == a {
		t.Fatalf("two servers share the file %s for the same ID", filepath.Base(a))
	}
}
