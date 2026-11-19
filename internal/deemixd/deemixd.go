// Package deemixd is enginex-deemixd: it downloads directly from Deezer on the
// deck for a queue on 127.0.0.1:6595. enginex-suite runs Main.
package deemixd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/deezer"
	"github.com/Cd1s/EngineX-OS/internal/stems"
)

const (
	defaultStateDir    = "/data/enginex/state"
	defaultDownloadDir = "/data/enginex/Stream"
	sdDownloadDir      = "/media/enginex-sd-cache/Music/Stream"
	sdDatabasePath     = "/media/enginex-sd-cache/Engine Library/Database2/m.db"
	defaultRelay       = ""
	defaultPassword    = "change-me-in-config"
	maxJobs            = 200
	maxPending         = 20
	sessionTTL         = 15 * time.Minute
)

var (
	errURLRe    = regexp.MustCompile(`(?i)https?://\S+`)
	errSecretRe = regexp.MustCompile(`\b(?:[0-9a-fA-F]{32,}|eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_.-]+)\b`)
)

func sanitizeError(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		return "network request failed"
	}
	msg := errURLRe.ReplaceAllString(err.Error(), "<url>")
	msg = errSecretRe.ReplaceAllString(msg, "<redacted>")
	return msg
}

type queueRequest struct {
	URL        string `json:"url"`
	Bitrate    int    `json:"bitrate"`
	Quality    string `json:"quality"`
	AllowLossy bool   `json:"allowLossy"`
}

type job struct {
	ID       string  `json:"id"`
	State    string  `json:"state"`
	Progress float64 `json:"progress"`
	Path     string  `json:"path,omitempty"`
	Error    string  `json:"error,omitempty"`
}

type downloadFunc func(context.Context, string, string, string, deezer.DownloadOptions) (string, error)

type daemon struct {
	stateDir, downloadDir string
	hc                    *http.Client
	login                 func(context.Context, string) (*deezer.Session, error)
	downloadTrack         func(context.Context, *deezer.Session, string, string, deezer.DownloadOptions) (string, error)
	download              downloadFunc
	ctx                   context.Context
	sem                   chan struct{}
	sessMu                sync.Mutex
	sessARL               string
	sess                  *deezer.Session
	sessAt                time.Time
	mu                    sync.Mutex
	jobs                  map[string]job
	order                 []string
	wg                    sync.WaitGroup
}

func newDaemon(ctx context.Context, stateDir, downloadDir string) *daemon {
	d := &daemon{
		stateDir: stateDir, downloadDir: downloadDir, ctx: ctx,
		hc:    &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		login: deezer.Login,
		downloadTrack: func(ctx context.Context, s *deezer.Session, trackID, dst string, opts deezer.DownloadOptions) (string, error) {
			return s.DownloadTrack(ctx, trackID, dst, opts)
		},
		sem:  make(chan struct{}, 1),
		jobs: make(map[string]job),
	}
	d.download = d.downloadDirect
	return d
}

func preferredDownloadDir() string {
	return preferredDownloadDirFrom(sdDownloadDir, sdDatabasePath, defaultDownloadDir)
}

// preferredDownloadDirFrom matches stream-sync's SD-cache rule: only use the SD
// cache directory when the Engine Library database on that card is also present,
// otherwise fall back to internal storage so deemixd and stream-sync agree.
func preferredDownloadDirFrom(sdDir, sdDB, fallback string) string {
	if st, err := os.Stat(sdDir); err == nil && st.IsDir() {
		if db, err := os.Stat(sdDB); err == nil && db.Mode().IsRegular() {
			return sdDir
		}
	}
	return fallback
}

// Main runs the daemon until SIGINT/SIGTERM.
func Main(args []string) error {
	fs := flag.NewFlagSet("enginex-deemixd", flag.ContinueOnError)
	downloadDir := fs.String("download-dir", preferredDownloadDir(), "directory for original FLAC/MP3 downloads")
	stateDir := fs.String("state-dir", defaultStateDir, "EngineX state directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	absDir, err := filepath.Abs(*downloadDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(absDir, 0755); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	d := newDaemon(ctx, *stateDir, absDir)
	srv := &http.Server{Addr: "127.0.0.1:6595", Handler: d.routes(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second}
	listener, err := net.Listen("tcp", srv.Addr)
	if errors.Is(err, syscall.EADDRINUSE) {
		// stream-sync already supervises a downloader here (#159). Exit 0 so
		// systemd's Restart=on-failure does not respawn this every 3 s.
		log.Printf("enginex-deemixd: %s already served; exiting", srv.Addr)
		return nil
	}
	if err != nil {
		return err
	}
	// Binding first ensures another live daemon cannot have its active partial
	// downloads removed by a second instance starting on the same directory.
	if err := cleanupPartials(absDir); err != nil {
		_ = listener.Close()
		return err
	}
	shutdownDone := make(chan struct{})
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
		close(shutdownDone)
	}()
	log.Printf("enginex-deemixd listening on %s; downloads: %s", srv.Addr, absDir)
	if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-shutdownDone
	// Shutdown has stopped handlers from adding jobs; cancelled workers must
	// finish their deferred file cleanup before the process exits.
	d.wg.Wait()
	return nil
}

func respond(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (d *daemon) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { respond(w, 200, map[string]bool{"ok": true}) })
	mux.HandleFunc("POST /api/queue", d.enqueue)
	mux.HandleFunc("GET /api/queue/{id}", d.poll)
	return mux
}

var trackPath = regexp.MustCompile(`^/(?:[a-z]{2}/)?track/([1-9][0-9]{0,19})/?$`)

func parseRequest(req queueRequest) (string, deezer.DownloadOptions, error) {
	u, err := url.Parse(req.URL)
	if err != nil || u.Scheme != "https" || (u.Host != "www.deezer.com" && u.Host != "deezer.com") || u.User != nil {
		return "", deezer.DownloadOptions{}, errors.New("url must be an HTTPS Deezer track URL")
	}
	m := trackPath.FindStringSubmatch(u.Path)
	if m == nil {
		return "", deezer.DownloadOptions{}, errors.New("url must identify a Deezer track")
	}
	bitrate := req.Bitrate
	switch strings.ToUpper(strings.TrimSpace(req.Quality)) {
	case "":
	case "FLAC":
		if bitrate == 0 {
			bitrate = 9
		} else if bitrate != 9 {
			return "", deezer.DownloadOptions{}, errors.New("quality and bitrate disagree")
		}
	case "MP3_320", "320":
		if bitrate == 0 {
			bitrate = 3
		} else if bitrate != 3 {
			return "", deezer.DownloadOptions{}, errors.New("quality and bitrate disagree")
		}
	case "MP3_128", "128":
		if bitrate == 0 {
			bitrate = 1
		} else if bitrate != 1 {
			return "", deezer.DownloadOptions{}, errors.New("quality and bitrate disagree")
		}
	case "MP3":
		if bitrate == 0 {
			bitrate = 3
		} else if bitrate != 1 && bitrate != 3 {
			return "", deezer.DownloadOptions{}, errors.New("quality and bitrate disagree")
		}
	default:
		return "", deezer.DownloadOptions{}, errors.New("unsupported quality")
	}
	if bitrate == 0 {
		bitrate = 9
	}
	if bitrate != 9 && bitrate != 3 && bitrate != 1 {
		return "", deezer.DownloadOptions{}, errors.New("bitrate must be 9, 3, or 1")
	}
	return m[1], deezer.DownloadOptions{Bitrate: bitrate, AllowLossy: req.AllowLossy}, nil
}

func (d *daemon) enqueue(w http.ResponseWriter, r *http.Request) {
	var req queueRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	if err := dec.Decode(&req); err != nil {
		respond(w, 400, map[string]string{"error": "invalid JSON"})
		return
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		respond(w, 400, map[string]string{"error": "expected one JSON object"})
		return
	}
	trackID, opts, err := parseRequest(req)
	if err != nil {
		respond(w, 400, map[string]string{"error": err.Error()})
		return
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		respond(w, 500, map[string]string{"error": "cannot allocate job"})
		return
	}
	j := job{ID: hex.EncodeToString(nonce[:]), State: "downloading", Progress: 0.1}
	d.mu.Lock()
	pending := 0
	for _, x := range d.jobs {
		if x.State == "downloading" {
			pending++
		}
	}
	if pending >= maxPending {
		d.mu.Unlock()
		respond(w, 429, map[string]string{"error": "download queue full"})
		return
	}
	// Remove only terminal jobs, so all active IDs remain pollable.
	for len(d.jobs) >= maxJobs {
		for i, id := range d.order {
			if d.jobs[id].State != "downloading" {
				delete(d.jobs, id)
				d.order = append(d.order[:i], d.order[i+1:]...)
				break
			}
		}
	}
	d.jobs[j.ID] = j
	d.order = append(d.order, j.ID)
	d.mu.Unlock()
	d.wg.Add(1)
	go func() { defer d.wg.Done(); d.run(j.ID, trackID, opts) }()
	respond(w, http.StatusAccepted, j)
}

func (d *daemon) poll(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	j, ok := d.jobs[r.PathValue("id")]
	d.mu.Unlock()
	if !ok {
		respond(w, 404, map[string]string{"error": "unknown job"})
		return
	}
	respond(w, 200, j)
}

func (d *daemon) run(id, trackID string, opts deezer.DownloadOptions) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(d.ctx, 10*time.Minute)
	defer cancel()
	var path string
	var err error
	select {
	case d.sem <- struct{}{}:
		defer func() { <-d.sem }()
		path, err = d.fetch(ctx, trackID, opts)
	case <-ctx.Done():
		err = ctx.Err()
	}
	d.mu.Lock()
	j := d.jobs[id]
	if err != nil {
		j.State = "error"
		j.Error = sanitizeError(err)
	} else {
		j.State = "completed"
		j.Progress = 1
		j.Path = path
	}
	d.jobs[id] = j
	d.mu.Unlock()
	if err != nil {
		log.Printf("enginex-deemixd: track=%s state=error elapsed=%s err=%q", trackID, time.Since(started).Round(time.Millisecond), j.Error)
	} else {
		log.Printf("enginex-deemixd: track=%s state=completed elapsed=%s", trackID, time.Since(started).Round(time.Millisecond))
	}
}

func (d *daemon) fetch(ctx context.Context, trackID string, opts deezer.DownloadOptions) (string, error) {
	arl, fromLocal, err := d.credentialsSource(ctx)
	if err != nil {
		return "", err
	}
	if st, err := os.Stat(d.downloadDir); err != nil || !st.IsDir() {
		return "", errors.New("download directory unavailable")
	}
	f, err := os.CreateTemp(d.downloadDir, ".deemix-"+trackID+"-*.part")
	if err != nil {
		return "", err
	}
	tmp := f.Name()
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	defer os.Remove(tmp)
	format, err := d.download(ctx, arl, trackID, tmp, opts)
	if err != nil && fromLocal && errors.Is(err, deezer.ErrInvalidARL) {
		if freshARL, rerr := d.refreshExpiredARLFromRelay(ctx, arl); rerr == nil && freshARL != "" {
			format, err = d.download(ctx, freshARL, trackID, tmp, opts)
		}
	}
	if err != nil {
		return "", err
	}
	if format != "flac" && format != "mp3" {
		return "", errors.New("unsupported downloaded audio format")
	}
	st, err := os.Stat(tmp)
	if err != nil {
		return "", err
	}
	if st.Size() == 0 {
		return "", errors.New("download produced empty audio")
	}
	if err := os.Chmod(tmp, 0644); err != nil {
		// FAT/exFAT may expose permissions from the mount mask and reject
		// chmod. Accept that only when the file is already player-readable.
		st, statErr := os.Stat(tmp)
		if (!errors.Is(err, syscall.EPERM) && !errors.Is(err, syscall.EOPNOTSUPP)) || statErr != nil || st.Mode().Perm()&0444 != 0444 {
			return "", err
		}
	}
	path := filepath.Join(d.downloadDir, trackID+"."+format)
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	return path, nil
}

func (d *daemon) downloadDirect(ctx context.Context, arl, trackID, dst string, opts deezer.DownloadOptions) (string, error) {
	d.sessMu.Lock()
	cached := d.sess != nil && d.sessARL == arl && time.Since(d.sessAt) < sessionTTL
	sess := d.sess
	d.sessMu.Unlock()
	if !cached {
		var err error
		sess, err = d.login(ctx, arl)
		if err != nil {
			return "", err
		}
		d.sessMu.Lock()
		d.sess, d.sessARL, d.sessAt = sess, arl, time.Now()
		d.sessMu.Unlock()
	}
	format, err := d.downloadTrack(ctx, sess, trackID, dst, opts)
	if err != nil && cached && ctx.Err() == nil {
		d.sessMu.Lock()
		if d.sess == sess {
			d.sess, d.sessARL, d.sessAt = nil, "", time.Time{}
		}
		d.sessMu.Unlock()
		sess, lerr := d.login(ctx, arl)
		if lerr != nil {
			return "", lerr
		}
		d.sessMu.Lock()
		d.sess, d.sessARL, d.sessAt = sess, arl, time.Now()
		d.sessMu.Unlock()
		return d.downloadTrack(ctx, sess, trackID, dst, opts)
	}
	return format, err
}

func readObject(path string) (map[string]json.RawMessage, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return make(map[string]json.RawMessage), nil
	}
	if err != nil {
		return nil, err
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(b, &obj) != nil || obj == nil {
		return nil, fmt.Errorf("invalid state JSON: %s", filepath.Base(path))
	}
	return obj, nil
}

func (d *daemon) relayFetchARL(ctx context.Context) (arl, userID string, err error) {
	cfg, err := readObject(filepath.Join(d.stateDir, "deemix-config.json"))
	if err != nil {
		return "", "", err
	}
	relay, password := defaultRelay, defaultPassword
	for _, key := range []string{"deemixUrl", "relayUrl"} {
		if raw, ok := cfg[key]; ok {
			var s string
			if json.Unmarshal(raw, &s) != nil {
				return "", "", fmt.Errorf("invalid %s in deemix-config.json", key)
			}
			if s != "" {
				relay = s
			}
		}
	}
	if raw, ok := cfg["deemixPassword"]; ok {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return "", "", errors.New("invalid deemixPassword in deemix-config.json")
		}
		if s != "" {
			password = s
		}
	}
	u, err := url.Parse(relay)
	if err != nil || u.Host == "" || u.User != nil {
		return "", "", errors.New("invalid deemixUrl")
	}
	if !stems.ValidEndpoint(relay) {
		return "", "", errors.New("deemixUrl must use HTTPS or a private LAN address")
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/api/config"
	u.RawPath = ""
	u.RawQuery = "secrets=1"
	u.Fragment = ""
	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return "", "", errors.New("invalid relay request")
	}
	req.Header.Set("X-Deemix-Password", password)
	req.Header.Set("Authorization", "Bearer "+password)
	resp, err := d.hc.Do(req)
	if err != nil {
		return "", "", errors.New("credential relay connection failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", "", fmt.Errorf("credential relay: HTTP %d", resp.StatusCode)
	}
	var result struct {
		Config struct {
			ARL    string `json:"deezerArl"`
			UserID string `json:"deezerUserId"`
		} `json:"config"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result) != nil || !deezer.ValidARL(result.Config.ARL) {
		return "", "", errors.New("credential relay returned no valid deezerArl")
	}
	return result.Config.ARL, result.Config.UserID, nil
}

func (d *daemon) credentials(ctx context.Context) (string, error) {
	arl, _, err := d.credentialsSource(ctx)
	return arl, err
}

func (d *daemon) credentialsSource(ctx context.Context) (string, bool, error) {
	ownerPath := filepath.Join(d.stateDir, "deezer-owner.json")
	obj, err := readObject(ownerPath)
	if err != nil {
		return "", false, err
	}
	var arl string
	if raw, ok := obj["ARL"]; ok {
		if json.Unmarshal(raw, &arl) != nil {
			return "", false, errors.New("invalid ARL in deezer-owner.json")
		}
	}
	arl = strings.TrimSpace(arl)
	if arl != "" {
		if !deezer.ValidARL(arl) {
			return "", false, errors.New("invalid ARL in deezer-owner.json")
		}
		return arl, true, nil
	}
	relayARL, relayUserID, err := d.relayFetchARL(ctx)
	if err != nil {
		return "", false, err
	}
	// Re-read before publishing to preserve fields written by stream-sync while
	// the relay request was in flight, including a newly supplied local ARL.
	obj, err = readObject(ownerPath)
	if err != nil {
		return "", false, err
	}
	var current string
	if raw, ok := obj["ARL"]; ok {
		_ = json.Unmarshal(raw, &current)
	}
	if deezer.ValidARL(strings.TrimSpace(current)) {
		return strings.TrimSpace(current), true, nil
	}
	obj["ARL"], _ = json.Marshal(relayARL)
	userRaw, hasUser := obj["UserID"]
	if (!hasUser || string(userRaw) == `""` || string(userRaw) == "null") && relayUserID != "" {
		obj["UserID"], _ = json.Marshal(relayUserID)
	}
	b, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		return "", false, err
	}
	if err := writeAtomic(ownerPath, b); err != nil {
		return "", false, err
	}
	return relayARL, false, nil
}

// refreshExpiredARLFromRelay pulls a newer ARL from the configured relay when a
// locally saved ARL was rejected by Deezer (USER_ID=0).
func (d *daemon) refreshExpiredARLFromRelay(ctx context.Context, expiredARL string) (string, error) {
	relayARL, relayUserID, err := d.relayFetchARL(ctx)
	if err != nil || relayARL == expiredARL {
		return "", err
	}
	ownerPath := filepath.Join(d.stateDir, "deezer-owner.json")
	obj, err := readObject(ownerPath)
	if err != nil {
		return "", err
	}
	var current string
	if raw, ok := obj["ARL"]; ok {
		_ = json.Unmarshal(raw, &current)
	}
	current = strings.TrimSpace(current)
	if current != "" && current != expiredARL && deezer.ValidARL(current) {
		return current, nil
	}
	obj["ARL"], _ = json.Marshal(relayARL)
	if relayUserID != "" {
		obj["UserID"], _ = json.Marshal(relayUserID)
	}
	b, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		return "", err
	}
	if err := writeAtomic(ownerPath, b); err != nil {
		return "", err
	}
	return relayARL, nil
}

func writeAtomic(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".deezer-owner-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err = f.Write(b); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

var partialName = regexp.MustCompile(`^\.deemix-[1-9][0-9]{0,19}-[A-Za-z0-9]+\.part$`)

func cleanupPartials(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !partialName.MatchString(entry.Name()) || !entry.Type().IsRegular() {
			continue
		}
		if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}
