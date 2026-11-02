// Command sc5000-deemixd downloads directly from Deezer on the SC5000.
package main

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

	"github.com/Cd1s/sc5000-custom-os/internal/deezer"
)

const (
	defaultStateDir    = "/data/sc5000-custom/state"
	defaultDownloadDir = "/data/sc5000-custom/Stream"
	sdDownloadDir      = "/media/sc5000-sd-cache/Music/Stream"
	defaultRelay       = "https://denon.cheek.day"
	defaultPassword    = "change-me-in-config"
	maxJobs            = 200
	maxPending         = 20
)

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
	download              downloadFunc
	ctx                   context.Context
	sem                   chan struct{}
	mu                    sync.Mutex
	jobs                  map[string]job
	order                 []string
	wg                    sync.WaitGroup
}

func newDaemon(ctx context.Context, stateDir, downloadDir string) *daemon {
	return &daemon{stateDir: stateDir, downloadDir: downloadDir, ctx: ctx,
		hc:       &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		download: downloadDirect, sem: make(chan struct{}, 1), jobs: make(map[string]job)}
}

func preferredDownloadDir() string {
	if st, err := os.Stat(sdDownloadDir); err == nil && st.IsDir() {
		return sdDownloadDir
	}
	return defaultDownloadDir
}

func main() {
	downloadDir := flag.String("download-dir", preferredDownloadDir(), "directory for original FLAC/MP3 downloads")
	stateDir := flag.String("state-dir", defaultStateDir, "SC5000 state directory")
	flag.Parse()
	absDir, err := filepath.Abs(*downloadDir)
	if err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(absDir, 0755); err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	d := newDaemon(ctx, *stateDir, absDir)
	srv := &http.Server{Addr: "127.0.0.1:6595", Handler: d.routes(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second}
	listener, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		log.Fatal(err)
	}
	// Binding first ensures another live daemon cannot have its active partial
	// downloads removed by a second instance starting on the same directory.
	if err := cleanupPartials(absDir); err != nil {
		_ = listener.Close()
		log.Fatal(err)
	}
	shutdownDone := make(chan struct{})
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
		close(shutdownDone)
	}()
	log.Printf("sc5000-deemixd listening on %s; downloads: %s", srv.Addr, absDir)
	if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	<-shutdownDone
	// Shutdown has stopped handlers from adding jobs; cancelled workers must
	// finish their deferred file cleanup before the process exits.
	d.wg.Wait()
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
	defer d.mu.Unlock()
	j := d.jobs[id]
	if err != nil {
		j.State = "error"
		// URL errors can contain signed media URLs or Deezer API tokens.
		var ue *url.Error
		if errors.As(err, &ue) {
			j.Error = "network request failed"
		} else {
			j.Error = err.Error()
		}
	} else {
		j.State = "completed"
		j.Progress = 1
		j.Path = path
	}
	d.jobs[id] = j
}

func (d *daemon) fetch(ctx context.Context, trackID string, opts deezer.DownloadOptions) (string, error) {
	arl, err := d.credentials(ctx)
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

func downloadDirect(ctx context.Context, arl, trackID, dst string, opts deezer.DownloadOptions) (string, error) {
	session, err := deezer.Login(ctx, arl)
	if err != nil {
		return "", err
	}
	return session.DownloadTrack(ctx, trackID, dst, opts)
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

func (d *daemon) credentials(ctx context.Context) (string, error) {
	ownerPath := filepath.Join(d.stateDir, "deezer-owner.json")
	obj, err := readObject(ownerPath)
	if err != nil {
		return "", err
	}
	var arl string
	if raw, ok := obj["ARL"]; ok {
		if json.Unmarshal(raw, &arl) != nil {
			return "", errors.New("invalid ARL in deezer-owner.json")
		}
	}
	arl = strings.TrimSpace(arl)
	if arl != "" {
		if !deezer.ValidARL(arl) {
			return "", errors.New("invalid ARL in deezer-owner.json")
		}
		return arl, nil
	}
	cfg, err := readObject(filepath.Join(d.stateDir, "deemix-config.json"))
	if err != nil {
		return "", err
	}
	relay, password := defaultRelay, defaultPassword
	for key, dst := range map[string]*string{"deemixUrl": &relay, "deemixPassword": &password} {
		if raw, ok := cfg[key]; ok {
			var s string
			if json.Unmarshal(raw, &s) != nil {
				return "", fmt.Errorf("invalid %s in deemix-config.json", key)
			}
			if s != "" {
				*dst = s
			}
		}
	}
	u, err := url.Parse(relay)
	if err != nil || u.Host == "" || u.User != nil {
		return "", errors.New("invalid deemixUrl")
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "https" && !(u.Scheme == "http" && ip != nil && ip.IsLoopback()) {
		return "", errors.New("deemixUrl must use HTTPS")
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/api/config"
	u.RawPath = ""
	u.RawQuery = "secrets=1"
	u.Fragment = ""
	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return "", errors.New("invalid relay request")
	}
	req.Header.Set("X-Deemix-Password", password)
	resp, err := d.hc.Do(req)
	if err != nil {
		return "", errors.New("credential relay connection failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("credential relay: HTTP %d", resp.StatusCode)
	}
	var result struct {
		Config struct {
			ARL    string `json:"deezerArl"`
			UserID string `json:"deezerUserId"`
		} `json:"config"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result) != nil || !deezer.ValidARL(result.Config.ARL) {
		return "", errors.New("credential relay returned no valid deezerArl")
	}
	// Re-read before publishing to preserve fields written by stream-sync while
	// the relay request was in flight, including a newly supplied local ARL.
	obj, err = readObject(ownerPath)
	if err != nil {
		return "", err
	}
	var current string
	if raw, ok := obj["ARL"]; ok {
		_ = json.Unmarshal(raw, &current)
	}
	if deezer.ValidARL(strings.TrimSpace(current)) {
		return strings.TrimSpace(current), nil
	}
	obj["ARL"], _ = json.Marshal(result.Config.ARL)
	userRaw, hasUser := obj["UserID"]
	if (!hasUser || string(userRaw) == `""` || string(userRaw) == "null") && result.Config.UserID != "" {
		obj["UserID"], _ = json.Marshal(result.Config.UserID)
	}
	b, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		return "", err
	}
	if err := writeAtomic(ownerPath, b); err != nil {
		return "", err
	}
	return result.Config.ARL, nil
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
