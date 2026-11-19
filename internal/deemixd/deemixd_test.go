package deemixd

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/deezer"
)

func testDaemon(t *testing.T) *daemon {
	t.Helper()
	return newDaemon(context.Background(), t.TempDir(), t.TempDir())
}
func writeState(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}
func enqueueTest(t *testing.T, h http.Handler, body string) job {
	t.Helper()
	r := httptest.NewRequest("POST", "/api/queue", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 202 {
		t.Fatalf("enqueue: %d %s", w.Code, w.Body)
	}
	var j job
	if err := json.Unmarshal(w.Body.Bytes(), &j); err != nil {
		t.Fatal(err)
	}
	if j.ID == "" || j.State != "downloading" || j.Progress != 0.1 {
		t.Fatalf("invalid start: %+v", j)
	}
	return j
}
func awaitJob(t *testing.T, h http.Handler, id string) job {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/api/queue/"+id, nil))
		if w.Code != 200 {
			t.Fatalf("poll: %d", w.Code)
		}
		var j job
		_ = json.Unmarshal(w.Body.Bytes(), &j)
		if j.State != "downloading" {
			return j
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("job timed out")
	return job{}
}

const trackRequest = `{"url":"https://www.deezer.com/track/3135556","bitrate":9,"quality":"FLAC","allowLossy":true}`

func TestQueueReadsLocalCredentialsEveryDownload(t *testing.T) {
	d := testDaemon(t)
	h := d.routes()
	owner := filepath.Join(d.stateDir, "deezer-owner.json")
	for _, format := range []string{"flac", "mp3"} {
		arl := strings.Repeat("a", 192)
		if format == "mp3" {
			arl = strings.Repeat("b", 192)
		}
		writeState(t, owner, `{"UserID":"6850245583","ARL":"`+arl+`"}`)
		d.download = func(ctx context.Context, got, trackID, path string, opts deezer.DownloadOptions) (string, error) {
			if got != arl || trackID != "3135556" || opts.Bitrate != 9 || !opts.AllowLossy {
				t.Error("wrong direct download parameters")
			}
			if filepath.Dir(path) != d.downloadDir {
				t.Error("audio must stay in download directory")
			}
			return format, os.WriteFile(path, []byte("original encoded audio"), 0600)
		}
		j := awaitJob(t, h, enqueueTest(t, h, trackRequest).ID)
		if j.State != "completed" || j.Progress != 1 || j.Path != filepath.Join(d.downloadDir, "3135556."+format) {
			t.Fatalf("completion: %+v", j)
		}
		st, err := os.Stat(j.Path)
		if err != nil || st.Mode().Perm() != 0644 {
			t.Fatal("audio must be readable by media player")
		}
		b, err := os.ReadFile(j.Path)
		if err != nil || string(b) != "original encoded audio" {
			t.Fatal("audio changed")
		}
	}
	ents, _ := os.ReadDir(d.downloadDir)
	if len(ents) != 2 {
		t.Fatal("temporary files remain")
	}
}

func TestCredentialSyncAndPreservation(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing", true: "empty"}[empty], func(t *testing.T) {
			d := testDaemon(t)
			arl := strings.Repeat("c", 192)
			calls := 0
			relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path != "/enginex/api/config" || r.URL.RawQuery != "secrets=1" || r.Header.Get("X-Deemix-Password") != "custom-password" {
					t.Error("incorrect relay request")
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"config": map[string]string{"deezerArl": arl, "deezerUserId": "6850245583"}})
			}))
			defer relay.Close()
			writeState(t, filepath.Join(d.stateDir, "deemix-config.json"), `{"deemixUrl":"`+relay.URL+`/enginex","deemixPassword":"custom-password"}`)
			owner := filepath.Join(d.stateDir, "deezer-owner.json")
			if empty {
				writeState(t, owner, `{"UserID":"local-id","ARL":"","extra":{"keep":true}}`)
			}
			for i := 0; i < 2; i++ {
				got, err := d.credentials(context.Background())
				if err != nil || got != arl {
					t.Fatalf("sync failed: %v", err)
				}
			}
			if calls != 1 {
				t.Fatalf("local ARL not reused: %d", calls)
			}
			obj, err := readObject(owner)
			if err != nil {
				t.Fatal(err)
			}
			var userID string
			_ = json.Unmarshal(obj["UserID"], &userID)
			if empty && (userID != "local-id" || string(obj["extra"]) != `{
    "keep": true
  }`) {
				var extra map[string]bool
				_ = json.Unmarshal(obj["extra"], &extra)
				if userID != "local-id" || !extra["keep"] {
					t.Fatal("owner fields lost")
				}
			} else if !empty && userID != "6850245583" {
				t.Fatal("relay UserID missing")
			}
			st, _ := os.Stat(owner)
			if st.Mode().Perm() != 0600 {
				t.Fatal("owner credentials not private")
			}
		})
	}
}

func TestDefaultRelayConfig(t *testing.T) {
	d := testDaemon(t)
	d.hc = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Error("unconfigured relay must not make network requests")
		return &http.Response{StatusCode: 200, Body: http.NoBody, Header: make(http.Header)}, nil
	})}
	if _, err := d.credentials(context.Background()); err == nil {
		t.Fatal("missing relay ARL accepted")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestFailureLeavesNoAudio(t *testing.T) {
	d := testDaemon(t)
	writeState(t, filepath.Join(d.stateDir, "deezer-owner.json"), `{"ARL":"`+strings.Repeat("a", 192)+`"}`)
	d.download = func(ctx context.Context, arl, id, path string, opts deezer.DownloadOptions) (string, error) {
		_ = os.WriteFile(path, []byte("partial"), 0600)
		return "", errors.New("source refused")
	}
	h := d.routes()
	j := awaitJob(t, h, enqueueTest(t, h, trackRequest).ID)
	if j.State != "error" || j.Error != "source refused" || j.Path != "" {
		t.Fatalf("error state: %+v", j)
	}
	ents, _ := os.ReadDir(d.downloadDir)
	if len(ents) != 0 {
		t.Fatal("partial file leaked")
	}
}

func TestBadRelayCannotOverwriteOwner(t *testing.T) {
	for _, status := range []int{401, 200} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			d := testDaemon(t)
			owner := filepath.Join(d.stateDir, "deezer-owner.json")
			original := `{"UserID":"keep","ARL":"","extra":7}`
			writeState(t, owner, original)
			relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"config":{"deezerArl":"invalid"}}`))
			}))
			defer relay.Close()
			writeState(t, filepath.Join(d.stateDir, "deemix-config.json"), `{"deemixUrl":"`+relay.URL+`"}`)
			if _, err := d.credentials(context.Background()); err == nil {
				t.Fatal("bad relay response accepted")
			}
			b, _ := os.ReadFile(owner)
			if string(b) != original {
				t.Fatal("owner modified on failure")
			}
		})
	}
}

func TestInvalidRequests(t *testing.T) {
	h := testDaemon(t).routes()
	for _, body := range []string{`{}`, `bad`, trackRequest + ` {}`, `{"url":"https://evil.example/track/1"}`, `{"url":"https://www.deezer.com/track/../1"}`, `{"url":"https://www.deezer.com/track/1","bitrate":9,"quality":"MP3"}`} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("POST", "/api/queue", strings.NewReader(body)))
		if w.Code != 400 {
			t.Errorf("invalid input accepted: %d", w.Code)
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/queue/no-such-job", nil))
	if w.Code != 404 {
		t.Fatal("missing job not 404")
	}
}

func TestConcurrentQueueBound(t *testing.T) {
	d := testDaemon(t)
	writeState(t, filepath.Join(d.stateDir, "deezer-owner.json"), `{"ARL":"`+strings.Repeat("a", 192)+`"}`)
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	d.download = func(ctx context.Context, arl, id, path string, opts deezer.DownloadOptions) (string, error) {
		<-release
		return "", errors.New("test stopped")
	}
	h := d.routes()
	var ids []string
	for i := 0; i < maxPending; i++ {
		ids = append(ids, enqueueTest(t, h, trackRequest).ID)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/api/queue", strings.NewReader(trackRequest)))
	if w.Code != 429 {
		t.Fatalf("queue bound: %d", w.Code)
	}
	once.Do(func() { close(release) })
	for _, id := range ids {
		awaitJob(t, h, id)
	}
}

func TestStartupCleansOnlyOwnPartials(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{".deemix-123-456.part", "123.flac", ".unrelated.part", ".deemix-README.part"} {
		writeState(t, filepath.Join(dir, name), "keep")
	}
	if err := cleanupPartials(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".deemix-123-456.part")); !os.IsNotExist(err) {
		t.Fatal("stale download remains")
	}
	for _, name := range []string{"123.flac", ".unrelated.part", ".deemix-README.part"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatal("unrelated file removed")
		}
	}
}

func TestCancelledWorkerCleansPartialBeforeExit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := newDaemon(ctx, t.TempDir(), t.TempDir())
	writeState(t, filepath.Join(d.stateDir, "deezer-owner.json"), `{"ARL":"`+strings.Repeat("a", 192)+`"}`)
	started := make(chan struct{})
	d.download = func(ctx context.Context, arl, id, path string, opts deezer.DownloadOptions) (string, error) {
		_ = os.WriteFile(path, []byte("partial"), 0600)
		close(started)
		<-ctx.Done()
		return "", ctx.Err()
	}
	h := d.routes()
	id := enqueueTest(t, h, trackRequest).ID
	<-started
	cancel()
	d.wg.Wait()
	j := awaitJob(t, h, id)
	if j.State != "error" {
		t.Fatal("cancelled worker did not fail")
	}
	ents, _ := os.ReadDir(d.downloadDir)
	if len(ents) != 0 {
		t.Fatal("cancelled worker left partial audio")
	}
}

func TestMissingDownloadDirectoryIsNotRecreated(t *testing.T) {
	d := testDaemon(t)
	writeState(t, filepath.Join(d.stateDir, "deezer-owner.json"), `{"ARL":"`+strings.Repeat("a", 192)+`"}`)
	d.downloadDir = filepath.Join(t.TempDir(), "unmounted-sd-stream")
	d.download = func(context.Context, string, string, string, deezer.DownloadOptions) (string, error) {
		t.Error("download started without directory")
		return "", errors.New("unexpected download")
	}
	h := d.routes()
	j := awaitJob(t, h, enqueueTest(t, h, trackRequest).ID)
	if j.State != "error" || j.Error != "download directory unavailable" {
		t.Fatalf("missing SD directory: %+v", j)
	}
	if _, err := os.Stat(d.downloadDir); !os.IsNotExist(err) {
		t.Fatal("missing SD directory was recreated")
	}
}

func TestMainExitsCleanlyWhenPortIsTaken(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:6595")
	if err != nil {
		t.Skipf("port 6595 unavailable: %v", err)
	}
	defer l.Close()
	if err := Main([]string{"-state-dir", t.TempDir(), "-download-dir", t.TempDir()}); err != nil {
		t.Fatalf("second instance must exit 0 so systemd does not respawn it: %v", err)
	}
}

func TestPreferredDownloadDirMatchesStreamSync(t *testing.T) {
	base := t.TempDir()
	sdDir := filepath.Join(base, "Music", "Stream")
	sdDB := filepath.Join(base, "Engine Library", "Database2", "m.db")
	fallback := filepath.Join(base, "internal")
	if err := os.MkdirAll(sdDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// SD stream folder exists, but Engine Library m.db is absent: must fall back
	// to internal storage so deemixd matches stream-sync.
	if got := preferredDownloadDirFrom(sdDir, sdDB, fallback); got != fallback {
		t.Fatalf("without m.db: got %q, want %q", got, fallback)
	}
	if err := os.MkdirAll(filepath.Dir(sdDB), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sdDB, []byte("sqlite"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := preferredDownloadDirFrom(sdDir, sdDB, fallback); got != sdDir {
		t.Fatalf("with m.db: got %q, want %q", got, sdDir)
	}
}

func TestSessionReusedAcrossDownloads(t *testing.T) {
	d := testDaemon(t)
	h := d.routes()
	arl1 := strings.Repeat("a", 192)
	arl2 := strings.Repeat("b", 192)
	owner := filepath.Join(d.stateDir, "deezer-owner.json")
	writeState(t, owner, `{"UserID":"42","ARL":"`+arl1+`"}`)

	logins := 0
	var lastLoginARL string
	d.login = func(_ context.Context, arl string) (*deezer.Session, error) {
		logins++
		lastLoginARL = arl
		return &deezer.Session{UserID: "42"}, nil
	}
	failFirstSecondTrack := true
	d.downloadTrack = func(_ context.Context, _ *deezer.Session, id, dst string, _ deezer.DownloadOptions) (string, error) {
		if id == "3135557" && failFirstSecondTrack {
			failFirstSecondTrack = false
			return "", errors.New("stale token")
		}
		return "flac", os.WriteFile(dst, []byte("fLaCdata"), 0o600)
	}

	// Track 1: logs in once.
	j := awaitJob(t, h, enqueueTest(t, h, trackRequest).ID)
	if j.State != "completed" || logins != 1 {
		t.Fatalf("first download: job=%+v logins=%d", j, logins)
	}
	// Track 1 again with same ARL: reuses cached session without re-logging in.
	j = awaitJob(t, h, enqueueTest(t, h, trackRequest).ID)
	if j.State != "completed" || logins != 1 {
		t.Fatalf("cached download should reuse session: job=%+v logins=%d", j, logins)
	}
	// Track 2 fails once on cached session -> invalidates cache, re-logins once, succeeds.
	j = awaitJob(t, h, enqueueTest(t, h, `{"url":"https://www.deezer.com/track/3135557","bitrate":9,"quality":"FLAC","allowLossy":true}`).ID)
	if j.State != "completed" || logins != 2 {
		t.Fatalf("stale cached session should re-login once: job=%+v logins=%d", j, logins)
	}
	// Changing local ARL invalidates previous session immediately.
	writeState(t, owner, `{"UserID":"42","ARL":"`+arl2+`"}`)
	j = awaitJob(t, h, enqueueTest(t, h, trackRequest).ID)
	if j.State != "completed" || logins != 3 || lastLoginARL != arl2 {
		t.Fatalf("new ARL should trigger fresh login: job=%+v logins=%d", j, logins)
	}
}

func TestExpiredLocalARLRefreshesFromRelay(t *testing.T) {
	d := testDaemon(t)
	h := d.routes()
	oldARL := strings.Repeat("d", 192)
	newARL := strings.Repeat("e", 192)
	owner := filepath.Join(d.stateDir, "deezer-owner.json")
	writeState(t, owner, `{"UserID":"42","ARL":"`+oldARL+`"}`)
	relayCalls := 0
	relaySrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		relayCalls++
		if r.Header.Get("X-Deemix-Password") != "pw" || r.Header.Get("Authorization") != "Bearer pw" {
			t.Errorf("missing relay auth headers")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"config":{"deezerArl":"` + newARL + `","deezerUserId":"99"}}`))
	}))
	defer relaySrv.Close()
	writeState(t, filepath.Join(d.stateDir, "deemix-config.json"), `{"relayUrl":"`+relaySrv.URL+`","deemixPassword":"pw"}`)
	d.hc = relaySrv.Client()

	d.login = func(_ context.Context, arl string) (*deezer.Session, error) {
		if arl == oldARL {
			return nil, deezer.ErrInvalidARL
		}
		if arl != newARL {
			t.Fatalf("unexpected ARL")
		}
		return &deezer.Session{UserID: "99"}, nil
	}
	d.downloadTrack = func(_ context.Context, _ *deezer.Session, _, dst string, _ deezer.DownloadOptions) (string, error) {
		return "flac", os.WriteFile(dst, []byte("fLaCdata"), 0o600)
	}

	j := awaitJob(t, h, enqueueTest(t, h, trackRequest).ID)
	if j.State != "completed" || relayCalls != 1 {
		t.Fatalf("expected relay refresh on expired ARL: job=%+v relayCalls=%d", j, relayCalls)
	}
	raw, err := os.ReadFile(owner)
	if err != nil {
		t.Fatal(err)
	}
	var saved struct {
		ARL    string
		UserID string
	}
	if err := json.Unmarshal(raw, &saved); err != nil || saved.ARL != newARL || saved.UserID != "99" {
		t.Fatalf("deezer-owner.json not updated with refreshed ARL/UserID")
	}
}

func TestSanitizeErrorRedactsURLsAndTokens(t *testing.T) {
	arl := strings.Repeat("a", 192)
	got := sanitizeError(errors.New("failed fetching https://cdn.deezer.com/secret?token=123 with " + arl))
	if strings.Contains(got, "cdn.deezer.com") || strings.Contains(got, arl) {
		t.Fatalf("secret leaked in sanitized error: %q", got)
	}
}
