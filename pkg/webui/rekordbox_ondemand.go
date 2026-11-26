package webui

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/bridge"
)

const (
	// RekordboxOnDemandRoute serves single-track direct-read RBXA1 performance payloads.
	RekordboxOnDemandRoute = "/v1/enginex/rekordbox/track-data"
	// DefaultRekordboxOnDemandSock is the local Unix domain socket for the Engine UI hook.
	DefaultRekordboxOnDemandSock = "/run/enginex/rb-ondemand.sock"
	rekordboxOnDemandSockEnv     = "ENGINEX_RB_ONDEMAND_SOCK"
)

type rbOnDemandRequest struct {
	DB     string  `json:"db,omitempty"`    // sqlite3_db_filename(db, "main")
	Mount  string  `json:"mount,omitempty"` // mounted volume root
	UUID   string  `json:"uuid,omitempty"`  // Information.uuid from m.db
	Path   string  `json:"path"`            // Track.path from m.db ("../Contents/...")
	ID     int64   `json:"id,omitempty"`    // Track.id from m.db
	Key    *int    `json:"key,omitempty"`   // Track.key (0..23) or omitted/null
	BPM    float64 `json:"bpm,omitempty"`   // Track.bpm / bpmAnalyzed
	Length int     `json:"length,omitempty"`
}

var onDemandChmod = os.Chmod

func (s *Server) rbOnDemandBuilder() *bridge.OnDemandBuilder {
	s.rbOnDemandMu.Lock()
	defer s.rbOnDemandMu.Unlock()
	if s.rbOnDemand == nil {
		mediaRoot := filepath.Clean(strings.TrimSpace(s.onDemandMediaRoot()))
		s.rbOnDemand = bridge.NewOnDemandBuilderWithConfig(bridge.OnDemandBuilderConfig{
			MediaRoot:         mediaRoot,
			RequireMountpoint: mediaRoot == "/media" || s.cfg.RequireOnDemandMountpoint,
		})
	}
	return s.rbOnDemand
}

func (s *Server) resolveOnDemandSockPath() string {
	if p := strings.TrimSpace(s.cfg.RekordboxOnDemandSock); p != "" {
		if p == "-" || strings.EqualFold(p, "off") {
			return ""
		}
		return p
	}
	if p := strings.TrimSpace(os.Getenv(rekordboxOnDemandSockEnv)); p != "" {
		if p == "-" || strings.EqualFold(p, "off") {
			return ""
		}
		return p
	}
	if testing.Testing() {
		return ""
	}
	return DefaultRekordboxOnDemandSock
}

func (s *Server) startRekordboxOnDemand() {
	sock := s.resolveOnDemandSockPath()
	if sock == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(sock), 0o755); err != nil {
		log.Printf("stream-sync: rb-ondemand mkdir %s: %v", filepath.Dir(sock), err)
		return
	}
	if st, err := os.Lstat(sock); err == nil {
		if st.Mode()&os.ModeSocket == 0 {
			log.Printf("stream-sync: rb-ondemand refusing non-socket path %s", sock)
			return
		}
		_ = os.Remove(sock)
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		log.Printf("stream-sync: rb-ondemand listen %s: %v", sock, err)
		return
	}
	if err := onDemandChmod(sock, 0o600); err != nil {
		log.Printf("stream-sync: rb-ondemand chmod %s: %v; refusing insecure socket", sock, err)
		_ = ln.Close()
		_ = os.Remove(sock)
		return
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+RekordboxOnDemandRoute, func(w http.ResponseWriter, r *http.Request) {
		s.serveRekordboxOnDemand(w, r, true)
	})
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 2 * time.Second,
		ReadTimeout:       3 * time.Second,
		WriteTimeout:      5 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	s.rbOnDemandMu.Lock()
	s.rbOnDemandLn = ln
	s.rbOnDemandSrv = srv
	s.rbOnDemandSock = sock
	s.rbOnDemandMu.Unlock()

	s.rbOnDemandWG.Add(1)
	go func() {
		defer s.rbOnDemandWG.Done()
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			log.Printf("stream-sync: rb-ondemand serve %s: %v", sock, err)
		}
	}()
}

func (s *Server) stopRekordboxOnDemand() {
	s.rbOnDemandMu.Lock()
	srv := s.rbOnDemandSrv
	ln := s.rbOnDemandLn
	sock := s.rbOnDemandSock
	s.rbOnDemandSrv = nil
	s.rbOnDemandLn = nil
	s.rbOnDemandSock = ""
	s.rbOnDemandMu.Unlock()

	if srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = srv.Shutdown(ctx)
		cancel()
	} else if ln != nil {
		_ = ln.Close()
	}
	s.rbOnDemandWG.Wait()
	if sock != "" {
		if st, err := os.Lstat(sock); err == nil && st.Mode()&os.ModeSocket != 0 {
			_ = os.Remove(sock)
		}
	}
}

func (s *Server) onDemandMediaRoot() string {
	if s.cfg.MediaRoot != "" {
		return s.cfg.MediaRoot
	}
	if s.shadow != nil && s.shadow.mediaRoot != "" {
		return s.shadow.mediaRoot
	}
	return s.mediaRoot()
}

// resolveOnDemandMount resolves and validates the stick mount root from req.Mount or req.DB,
// restricting access strictly to direct mounted volumes under mediaRoot.
func resolveOnDemandMount(mount, dbPath, mediaRoot string) (string, bool) {
	if mediaRoot == "" {
		return "", false
	}
	cand := strings.TrimSpace(mount)
	if cand == "" {
		dbClean := filepath.Clean(strings.TrimSpace(dbPath))
		if filepath.Base(dbClean) != "m.db" {
			return "", false
		}
		dbDir := filepath.Dir(dbClean)
		engDir := filepath.Dir(dbDir)
		if filepath.Base(dbDir) != "Database2" || filepath.Base(engDir) != "Engine Library" {
			return "", false
		}
		cand = filepath.Dir(engDir)
	}
	cleanRoot := filepath.Clean(mediaRoot)
	cleanMount := filepath.Clean(cand)
	rel, err := filepath.Rel(cleanRoot, cleanMount)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, "..") {
		return "", false
	}
	if strings.ContainsAny(filepath.ToSlash(rel), "/") {
		return "", false
	}
	st, err := os.Lstat(cleanMount)
	if err != nil || !st.Mode().IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return "", false
	}
	return cleanMount, true
}

func (s *Server) handleRekordboxOnDemand(w http.ResponseWriter, r *http.Request) {
	s.serveRekordboxOnDemand(w, r, false)
}

func (s *Server) serveRekordboxOnDemand(w http.ResponseWriter, r *http.Request, fromUnix bool) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	if !fromUnix && !writeBackFromDeck(r) {
		fail(w, http.StatusForbidden, errors.New("loopback only"))
		return
	}
	var req rbOnDemandRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if strings.TrimSpace(req.Path) == "" {
		fail(w, http.StatusBadRequest, errors.New("missing path"))
		return
	}
	mount, ok := resolveOnDemandMount(req.Mount, req.DB, s.onDemandMediaRoot())
	if !ok {
		w.Header().Set("X-RB-Status", "not_found")
		fail(w, http.StatusNotFound, errors.New("volume not found"))
		return
	}

	// Yield any active background pass cleanly (zero-cost no-op when no pass is running).
	s.rbPriorityQueue().NoteLoading(time.Now())

	engineKey := -1
	if req.Key != nil {
		engineKey = *req.Key
	}
	res, err := s.rbOnDemandBuilder().Build(r.Context(), bridge.OnDemandRequest{
		Mount:      mount,
		UUID:       strings.TrimSpace(req.UUID),
		EnginePath: req.Path,
		EngineID:   req.ID,
		EngineKey:  engineKey,
		EngineBPM:  req.BPM,
		EngineLen:  req.Length,
	})
	if err != nil {
		switch {
		case errors.Is(err, bridge.ErrOnDemandNotFound):
			w.Header().Set("X-RB-Status", "not_found")
			fail(w, http.StatusNotFound, err)
		case errors.Is(err, bridge.ErrOnDemandUUIDMismatch):
			w.Header().Set("X-RB-Status", "uuid_mismatch")
			fail(w, http.StatusConflict, err)
		case errors.Is(err, bridge.ErrOnDemandDegraded), errors.Is(err, bridge.ErrOnDemandLimit):
			w.Header().Set("X-RB-Status", "degraded")
			fail(w, http.StatusUnprocessableEntity, err)
		case errors.Is(err, bridge.ErrOnDemandUnavailable):
			w.Header().Set("X-RB-Status", "unavailable")
			fail(w, http.StatusServiceUnavailable, err)
		default:
			w.Header().Set("X-RB-Status", "error")
			fail(w, http.StatusServiceUnavailable, err)
		}
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(res.RBXA1)))
	w.Header().Set("X-RB-Key", res.Key)
	w.Header().Set("X-RB-Written", base64.StdEncoding.EncodeToString(res.Written))
	w.Header().Set("X-RB-Written-NoMain", base64.StdEncoding.EncodeToString(res.WrittenNoMain))
	w.Header().Set("X-RB-Row", res.RowHash)
	w.Header().Set("X-RB-Active-Loops", strconv.FormatUint(uint64(res.ActiveLoops), 10))
	w.Header().Set("X-RB-Rules", strconv.Itoa(res.Rules))
	w.Header().Set("X-RB-Timing-Ms", fmt.Sprintf("%.2f", res.Timing.TotalMS))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(res.RBXA1)
}
