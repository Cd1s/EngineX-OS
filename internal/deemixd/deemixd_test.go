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
				if r.URL.Path != "/sc5000/api/config" || r.URL.RawQuery != "secrets=1" || r.Header.Get("X-Deemix-Password") != "custom-password" {
					t.Error("incorrect relay request")
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"config": map[string]string{"deezerArl": arl, "deezerUserId": "6850245583"}})
			}))
			defer relay.Close()
			writeState(t, filepath.Join(d.stateDir, "deemix-config.json"), `{"deemixUrl":"`+relay.URL+`/sc5000","deemixPassword":"custom-password"}`)
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
