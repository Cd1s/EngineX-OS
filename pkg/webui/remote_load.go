package webui

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	remotePendingTTL = 10 * time.Second // a request nobody picked up
	remoteLoadingTTL = 8 * time.Second  // Go backstop for a lost confirmation, from QML's last mention
	remoteStateTTL   = 2 * time.Second  // how long reported deck state counts as fresh
	remoteKeep       = 20
)

type remoteDeck struct {
	Deck    int    `json:"deck"`
	Side    string `json:"side"`
	Local   bool   `json:"local"`
	Loaded  bool   `json:"loaded"`
	Playing bool   `json:"playing"`
	Title   string `json:"title,omitempty"`
	Artist  string `json:"artist,omitempty"`
}

type remoteLoadRequest struct {
	ID    string `json:"id"`
	Deck  int    `json:"deck"`
	Path  string `json:"path"` // relative, as the client sent it
	Drive string `json:"drive"`
	File  string `json:"-"` // absolute, resolved; sent to QML only
	// DriveRel is the resolved file relative to its drive; sent to QML only.
	DriveRel string    `json:"-"`
	Status   string    `json:"status"`
	Reason   string    `json:"reason,omitempty"`
	At       time.Time `json:"-"` // last status change
}

type remoteLoadUpdate struct{ ID, Status, Reason string }

type remoteLoader struct {
	mu      sync.Mutex
	now     func() time.Time
	board   string
	decks   []remoteDeck
	stateAt time.Time
	// lock is Engine's "Lock Playing Deck" preference as QML last read it, nil when unknown.
	lock *bool
	reqs []remoteLoadRequest
}

func newRemoteLoader(now func() time.Time) *remoteLoader {
	return &remoteLoader{now: now}
}

func (l *remoteLoader) setBoard(board string) {
	l.mu.Lock()
	l.board = board
	l.mu.Unlock()
}

// remoteSide maps a deck number to the side of the SC LIVE 4 (JP21).
func remoteSide(board string, deck int) string {
	if board != "JP21" && board != "SCLIVE4" {
		return ""
	}
	switch deck {
	case 1, 3:
		return "left"
	case 2, 4:
		return "right"
	}
	return ""
}

// sweep ages requests. The QML poll stops when the switch is off, so every
// read path sweeps too. Caller holds l.mu.
func (l *remoteLoader) sweep(enabled bool) {
	now := l.now()
	for i := range l.reqs {
		r := &l.reqs[i]
		switch {
		case r.Status == "pending" && !enabled:
			r.Status, r.Reason, r.At = "rejected", "switched off", now
		case r.Status == "pending" && now.Sub(r.At) > remotePendingTTL:
			r.Status, r.At = "expired", now
		case r.Status == "loading" && now.Sub(r.At) > remoteLoadingTTL:
			r.Status, r.Reason, r.At = "failed", "no confirmation", now
		}
	}
}

// check runs the deck checks a load must pass. Caller holds l.mu.
func (l *remoteLoader) check(deck int, enabled bool) (int, error) {
	l.sweep(enabled)
	if !enabled {
		return http.StatusForbidden, errors.New("remote load is switched off")
	}
	if deck < 1 || deck > 4 {
		return http.StatusBadRequest, errors.New("deck must be 1-4")
	}
	if l.stateAt.IsZero() || l.now().Sub(l.stateAt) > remoteStateTTL {
		return http.StatusServiceUnavailable, errors.New("deck state unavailable")
	}
	var d *remoteDeck
	for i := range l.decks {
		if l.decks[i].Deck == deck {
			d = &l.decks[i]
		}
	}
	if d == nil {
		return http.StatusServiceUnavailable, errors.New("deck state unavailable")
	}
	if !d.Local {
		return http.StatusConflict, errors.New("deck locked")
	}
	if d.Playing {
		return http.StatusConflict, errors.New("deck playing")
	}
	for _, r := range l.reqs {
		if r.Deck == deck && (r.Status == "pending" || r.Status == "loading") {
			return http.StatusConflict, errors.New("deck has an open request")
		}
	}
	return http.StatusOK, nil
}

// precheck runs the deck checks before the file lookup, so a bad or busy deck
// is reported as such and not as a missing file. submit checks again.
func (l *remoteLoader) precheck(deck int, enabled bool) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.check(deck, enabled)
}

// submit queues a load. rel is the path as the client sent it, driveRel the
// resolved file relative to its drive: QML matches Engine's track name on it.
func (l *remoteLoader) submit(deck int, rel, drive, driveRel, file string, enabled bool) (*remoteLoadRequest, int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if code, err := l.check(deck, enabled); err != nil {
		return nil, code, err
	}
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	req := remoteLoadRequest{ID: hex.EncodeToString(b), Deck: deck, Path: rel, Drive: drive, DriveRel: driveRel, File: file, Status: "pending", At: l.now()}
	l.reqs = append(l.reqs, req)
	if len(l.reqs) > remoteKeep {
		l.reqs = append([]remoteLoadRequest(nil), l.reqs[len(l.reqs)-remoteKeep:]...)
	}
	out := req
	return &out, http.StatusAccepted, nil
}

// report takes the QML poll: deck states and results in, next pending request out.
func (l *remoteLoader) report(decks []remoteDeck, updates []remoteLoadUpdate, enabled bool) *remoteLoadRequest {
	return l.reportLock(decks, nil, "", updates, enabled)
}

// reportLock is report with Engine's "Lock Playing Deck" preference, which
// comes with the deck states (nil: unknown).
func (l *remoteLoader) reportLock(decks []remoteDeck, lock *bool, active string, updates []remoteLoadUpdate, enabled bool) *remoteLoadRequest {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	// active is the request QML still waits for (a slow load; QML gives up
	// after 60 s): it stays "loading", the backstop counts from this mention.
	if decks != nil && active != "" {
		for i := range l.reqs {
			if r := &l.reqs[i]; r.ID == active && r.Status == "loading" {
				r.At = now
			}
		}
	}
	l.sweep(enabled)
	if decks != nil {
		l.decks = make([]remoteDeck, len(decks))
		for i, d := range decks {
			d.Side = remoteSide(l.board, d.Deck)
			l.decks[i] = d
		}
		l.stateAt = now
		l.lock = lock
	}
	for _, u := range updates {
		if u.Status != "loaded" && u.Status != "rejected" && u.Status != "failed" {
			continue
		}
		for i := range l.reqs {
			if r := &l.reqs[i]; r.ID == u.ID && r.Status == "loading" {
				r.Status, r.Reason, r.At = u.Status, u.Reason, now
			}
		}
	}
	if !enabled {
		return nil
	}
	// One load at a time: QML confirms one track at once, so the next pending
	// request waits (within its TTL) until the one loading has a result.
	for _, r := range l.reqs {
		if r.Status == "loading" {
			return nil
		}
	}
	for i := range l.reqs {
		if r := &l.reqs[i]; r.Status == "pending" {
			r.Status, r.At = "loading", now
			out := *r
			return &out
		}
	}
	return nil
}

func (l *remoteLoader) snapshot(enabled bool) (decks []remoteDeck, stateAge time.Duration, reqs []remoteLoadRequest, board string, lock *bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweep(enabled)
	decks = append([]remoteDeck(nil), l.decks...)
	if !l.stateAt.IsZero() {
		stateAge = l.now().Sub(l.stateAt)
	}
	reqs = append([]remoteLoadRequest(nil), l.reqs...)
	board = l.board
	if l.lock != nil {
		v := *l.lock
		lock = &v
	}
	return
}

func (l *remoteLoader) get(id string, enabled bool) (remoteLoadRequest, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweep(enabled)
	for _, r := range l.reqs {
		if r.ID == id {
			return r, true
		}
	}
	return remoteLoadRequest{}, false
}

var remoteMountDevices = mountDevices // tests override

// remoteDrives lists the volumes a remote load may read: real block devices
// under the media root, not the internal storage, no network mounts.
func (s *Server) remoteDrives() map[string]string {
	root := s.mediaRoot()
	entries, _ := os.ReadDir(root)
	devs := remoteMountDevices()
	out := map[string]string{}
	for _, e := range entries {
		if !e.IsDir() || e.Type()&os.ModeSymlink != 0 || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		p := filepath.Join(root, e.Name())
		dev := devs[p]
		if !strings.HasPrefix(dev, "/dev/") {
			continue
		}
		sys, _ := filepath.EvalSymlinks(filepath.Join("/sys/class/block", filepath.Base(dev)))
		if driveKind(dev, sys) == "internal" {
			continue
		}
		out[e.Name()] = p
	}
	return out
}

// resolveRemoteFile finds rel on the eligible drives. driveRel is the resolved
// file relative to its drive's resolved root. status is the HTTP code for err;
// candidates names the drives when rel is ambiguous.
func (s *Server) resolveRemoteFile(rel, driveName string) (file, drive, driveRel string, candidates []string, status int, err error) {
	if rel == "" || strings.HasPrefix(rel, "/") || filepath.IsAbs(rel) || strings.ContainsRune(rel, 0) {
		return "", "", "", nil, http.StatusBadRequest, errors.New("path must be relative")
	}
	for _, part := range strings.Split(rel, "/") {
		if part == ".." {
			return "", "", "", nil, http.StatusBadRequest, errors.New("path must not contain ..")
		}
	}
	clean := filepath.Clean(rel)
	if clean == "." {
		return "", "", "", nil, http.StatusBadRequest, errors.New("path must name a file")
	}
	drives := s.remoteDrives()
	names := make([]string, 0, len(drives))
	for n := range drives {
		if driveName == "" || n == driveName {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	files, rels := map[string]string{}, map[string]string{}
	for _, n := range names {
		root, e := filepath.EvalSymlinks(drives[n])
		if e != nil {
			continue
		}
		f, e := filepath.EvalSymlinks(filepath.Join(root, clean))
		if e != nil {
			continue
		}
		r, e := filepath.Rel(root, f)
		if e != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
			continue
		}
		if !regularAudioFile(f) {
			continue
		}
		files[n], rels[n] = f, filepath.ToSlash(r)
		candidates = append(candidates, n)
	}
	switch len(candidates) {
	case 0:
		return "", "", "", nil, http.StatusNotFound, errors.New("file not found on any drive")
	case 1:
		n := candidates[0]
		return files[n], n, rels[n], nil, http.StatusOK, nil
	}
	return "", "", "", candidates, http.StatusConflict, errors.New("file exists on several drives")
}

// refuseTunnel rejects requests that a tunnel edge relayed (relayedFromOutside:
// Cloudflare, Tailscale Serve): the remote load API is for the local network
// only. It writes the 403.
func refuseTunnel(w http.ResponseWriter, r *http.Request) bool {
	if relayedFromOutside(r) {
		fail(w, http.StatusForbidden, errors.New("remote load is not available through the tunnel"))
		return true
	}
	return false
}

func (s *Server) remoteLoadStatus(w http.ResponseWriter, r *http.Request) {
	if refuseTunnel(w, r) {
		return
	}
	enabled := s.remoteLoadEnabled()
	decks, age, reqs, board, lock := s.remote.snapshot(enabled)
	if decks == nil {
		decks = []remoteDeck{}
	}
	out := make([]remoteLoadRequest, 0, len(reqs))
	for i := len(reqs) - 1; i >= 0; i-- { // newest first
		out = append(out, reqs[i])
	}
	respond(w, map[string]any{"enabled": enabled, "board": board, "decks": decks, "stateAgeMs": age.Milliseconds(),
		"lockPlayingDeck": lock, "requests": out})
}

func (s *Server) remoteLoadGet(w http.ResponseWriter, r *http.Request) {
	if refuseTunnel(w, r) {
		return
	}
	req, ok := s.remote.get(r.PathValue("id"), s.remoteLoadEnabled())
	if !ok {
		fail(w, http.StatusNotFound, errors.New("unknown request"))
		return
	}
	respond(w, req)
}

func (s *Server) remoteLoadSubmit(w http.ResponseWriter, r *http.Request) {
	if refuseTunnel(w, r) {
		return
	}
	var body struct {
		Deck  int    `json:"deck"`
		Path  string `json:"path"`
		Drive string `json:"drive"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&body); err != nil {
		fail(w, http.StatusBadRequest, errors.New("invalid JSON"))
		return
	}
	enabled := s.remoteLoadEnabled()
	if status, err := s.remote.precheck(body.Deck, enabled); err != nil {
		fail(w, status, err)
		return
	}
	file, drive, driveRel, candidates, status, err := s.resolveRemoteFile(body.Path, body.Drive)
	if err != nil {
		if status == http.StatusConflict && len(candidates) > 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			json.NewEncoder(w).Encode(map[string]any{"error": err.Error(), "candidates": candidates})
			return
		}
		fail(w, status, err)
		return
	}
	// driveRel is the file relative to its drive, as Engine will name it at the
	// end of the track's path (file:// or net://<id>/<volume>/<path>).
	req, status, err := s.remote.submit(body.Deck, body.Path, drive, driveRel, file, enabled)
	if err != nil {
		fail(w, status, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{"id": req.ID, "status": req.Status})
}

// remoteLoadPoll is the QML side: deck states and results in, the next
// pending request out. Loopback only (the gateway mux).
func (s *Server) remoteLoadPoll(w http.ResponseWriter, r *http.Request) {
	if refuseTunnel(w, r) {
		return
	}
	var body struct {
		Decks []remoteDeck `json:"decks"`
		// Engine's "Lock Playing Deck" preference; null when QML cannot read it.
		LockPlayingDeck *bool              `json:"lockPlayingDeck"`
		Updates         []remoteLoadUpdate `json:"updates"`
		// Active is the request QML still waits for, "" for none.
		Active string `json:"active"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&body); err != nil && err != io.EOF {
		fail(w, http.StatusBadRequest, errors.New("invalid JSON"))
		return
	}
	next := s.remote.reportLock(body.Decks, body.LockPlayingDeck, body.Active, body.Updates, s.remoteLoadEnabled())
	if next == nil {
		respond(w, map[string]any{"request": nil})
		return
	}
	respond(w, map[string]any{"request": map[string]any{"id": next.ID, "deck": next.Deck, "file": next.File, "path": next.DriveRel}})
}
