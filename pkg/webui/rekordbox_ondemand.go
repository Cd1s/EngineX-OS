package webui

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
)

const (
	// RekordboxOnDemandRoute serves single-track direct-read RBXA1 performance payloads.
	RekordboxOnDemandRoute = "/v1/enginex/rekordbox/track-data"
	// RekordboxOnDemandFilledRoute records that a remote Engine Link load committed a track fill.
	RekordboxOnDemandFilledRoute = "/v1/enginex/rekordbox/track-filled"
	// RekordboxPeerOnDemandRoute serves single-track RBXA1 payloads over the LAN to another EngineX deck.
	RekordboxPeerOnDemandRoute = "/peer/rekordbox/track-data"
	// RekordboxPeerFilledRoute notifies the stick owner to append filled.jsonl after a remote Link fill.
	RekordboxPeerFilledRoute = "/peer/rekordbox/track-filled"
	// DefaultRekordboxOnDemandSock is the local Unix domain socket for the Engine UI hook.
	DefaultRekordboxOnDemandSock = "/run/enginex/rb-ondemand.sock"
	rekordboxOnDemandSockEnv     = "ENGINEX_RB_ONDEMAND_SOCK"
)

type rbOnDemandRequest struct {
	DB        string  `json:"db,omitempty"`        // sqlite3_db_filename(db, "main")
	Mount     string  `json:"mount,omitempty"`     // mounted volume root
	UUID      string  `json:"uuid,omitempty"`      // Information.uuid from m.db
	Path      string  `json:"path"`                // Track.path from m.db ("../Contents/...")
	ID        int64   `json:"id,omitempty"`        // Track.id from m.db
	ImportKey int64   `json:"importKey,omitempty"` // Track.pdbImportKey from m.db
	Key       *int    `json:"key,omitempty"`       // Track.key (0..23) or omitted/null
	BPM       float64 `json:"bpm,omitempty"`       // Track.bpm / bpmAnalyzed
	Length    int     `json:"length,omitempty"`
	Repair    bool    `json:"repair,omitempty"`   // true when requesting repair of a stock-OA-overwritten row
	CurBeats  string  `json:"curBeats,omitempty"` // hex-encoded PerformanceData.beatData observed by hook
	CurCues   string  `json:"curCues,omitempty"`  // hex-encoded PerformanceData.quickCues observed by hook
	CurLoops  string  `json:"curLoops,omitempty"` // hex-encoded PerformanceData.loops observed by hook
}

var errOnDemandNotRepairable = errors.New("row is not an unedited stock-overwritten rekordbox row")

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
	mux.HandleFunc("POST "+RekordboxOnDemandFilledRoute, func(w http.ResponseWriter, r *http.Request) {
		s.serveRekordboxFilledNotify(w, r, true)
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

const (
	rbOnDemandCacheMax  = 8
	rbOnDemandCacheTTL  = 10 * time.Minute
	rbOnDemandDBMetaMax = 8
	rbOnDemandDBRowMax  = 32
)

// onDemandVerifiedID pins every on-demand cache entry and single-flight build to
// the mounted volume identity (dev/mntID/ino), snapshot generation, live Engine
// library identity (m.db dev/ino + Information.uuid), Device Library exports,
// resolved audio + ANLZ files, and canonical track metadata.
// Ordinary in-place m.db writes keep DBDev/DBIno/LiveUUID/track metadata intact
// and therefore reuse the cached build without invalidating it.
type onDemandVerifiedID struct {
	mount         string
	mountDev      uint64
	mountMntID    uint64
	mountIno      uint64
	snapshotUUID  string
	liveUUID      string
	snapFile      bridge.OnDemandFileID
	dbDev         uint64
	dbIno         uint64
	exportDB      bridge.OnDemandFileID
	exportPDB     bridge.OnDemandFileID
	audioFile     bridge.OnDemandFileID
	anlzFiles     [3]bridge.OnDemandFileID
	trackKey      string
	trackRel      string
	rowHash       string
	engineID      int64
	importKey     int64
	engineKey     int
	engineBPMBits uint64
	engineLen     int
}

type rbOnDemandDBKey struct {
	mountDev   uint64
	mountMntID uint64
	mountIno   uint64
	dbDev      uint64
	dbIno      uint64
	dbSize     int64
	dbModNS    int64
	walDev     uint64
	walIno     uint64
	walSize    int64
	walModNS   int64
}

type rbOnDemandTrackMeta struct {
	found     bool
	id        int64
	importKey int64
	key       int
	bpm       float64
	length    int
	analyzed  bool
}

type rbOnDemandDBSnap struct {
	uuid   string
	tracks map[string]rbOnDemandTrackMeta
	at     time.Time
}

type rbOnDemandCacheEntry struct {
	id       onDemandVerifiedID
	trackKey string
	res      *bridge.OnDemandResult
	enc      shadowEncodedPair
	at       time.Time
}

type rbOnDemandFlight struct {
	done     chan struct{}
	cancel   context.CancelFunc
	waiters  int
	trackKey string
	evicted  bool
	res      *bridge.OnDemandResult
	err      error
}

func onDemandCacheKey(mount, enginePath string) string {
	p := strings.ReplaceAll(strings.TrimSpace(enginePath), "\\", "/")
	p = strings.TrimPrefix(p, "../")
	clean := strings.TrimPrefix(path.Clean("/"+p), "/")
	if mount == "" || clean == "" || clean == "." {
		return ""
	}
	return filepath.Clean(mount) + "\x00" + rbstate.FoldPath(clean)
}

func (s *Server) canonicalOnDemandTrackKey(srcPath string) string {
	if srcPath == "" {
		return ""
	}
	mediaRoot := filepath.Clean(s.onDemandMediaRoot())
	sub, err := filepath.Rel(mediaRoot, filepath.Clean(srcPath))
	if err != nil || sub == "." || sub == ".." || strings.HasPrefix(sub, ".."+string(filepath.Separator)) {
		return ""
	}
	vol, rel, ok := strings.Cut(filepath.ToSlash(sub), "/")
	if !ok || vol == "" || rel == "" {
		return ""
	}
	return onDemandCacheKey(filepath.Join(mediaRoot, vol), "../"+rel)
}

func onDemandStatFile(p string) (dev, ino uint64, size, modNS int64, ok bool) {
	st, err := os.Stat(p)
	if err != nil || !st.Mode().IsRegular() {
		return 0, 0, 0, 0, false
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); ok && sys != nil {
		dev, ino = uint64(sys.Dev), uint64(sys.Ino)
	}
	return dev, ino, st.Size(), st.ModTime().UnixNano(), true
}

func (s *Server) lookupOnDemandDBMeta(ctx context.Context, stickID bridge.OnDemandStickIdentity) (string, rbOnDemandTrackMeta, error) {
	if !stickID.DBFile.Present {
		return "", rbOnDemandTrackMeta{}, bridge.ErrOnDemandNotFound
	}
	mdb := filepath.Join(stickID.Mount, "Engine Library", "Database2", "m.db")
	wDev, wIno, wSize, wModNS, _ := onDemandStatFile(mdb + "-wal")
	dbKey := rbOnDemandDBKey{
		mountDev:   stickID.MountDev,
		mountMntID: stickID.MountMntID,
		mountIno:   stickID.MountIno,
		dbDev:      stickID.DBFile.Dev,
		dbIno:      stickID.DBFile.Ino,
		dbSize:     stickID.DBFile.Size,
		dbModNS:    stickID.DBFile.ModNS,
		walDev:     wDev,
		walIno:     wIno,
		walSize:    wSize,
		walModNS:   wModNS,
	}
	s.rbOnDemandCacheMu.Lock()
	if snap := s.rbOnDemandDBMeta[dbKey]; snap != nil {
		if tm, ok := snap.tracks[stickID.TrackKey]; ok {
			uuid := snap.uuid
			snap.at = time.Now()
			s.rbOnDemandCacheMu.Unlock()
			return uuid, tm, nil
		}
	}
	s.rbOnDemandCacheMu.Unlock()

	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: mdb}).String()+"?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(500)")
	if err != nil {
		return "", rbOnDemandTrackMeta{}, bridge.ErrOnDemandUnavailable
	}
	defer db.Close()

	var liveUUID string
	if err := db.QueryRowContext(ctx, `SELECT coalesce(uuid, '') FROM Information LIMIT 1`).Scan(&liveUUID); err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", rbOnDemandTrackMeta{}, context.DeadlineExceeded
		}
		if errors.Is(err, sql.ErrNoRows) {
			return "", rbOnDemandTrackMeta{}, bridge.ErrOnDemandUUIDMismatch
		}
		return "", rbOnDemandTrackMeta{}, bridge.ErrOnDemandUnavailable
	}
	liveUUID = strings.TrimSpace(liveUUID)

	var tm rbOnDemandTrackMeta
	readRow := func(query string, args ...any) (bool, error) {
		var id, importKey int64
		var rowPath string
		var keyVal, lengthVal, analyzedVal int
		var bpmVal float64
		err := db.QueryRowContext(ctx, query, args...).Scan(&id, &rowPath, &importKey, &keyVal, &bpmVal, &lengthVal, &analyzedVal)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		clean := strings.TrimPrefix(strings.ReplaceAll(strings.TrimSpace(rowPath), "\\", "/"), "../")
		clean = strings.TrimPrefix(path.Clean("/"+clean), "/")
		if rbstate.FoldPath(clean) != stickID.TrackKey {
			return false, nil
		}
		tm = rbOnDemandTrackMeta{
			found:     true,
			id:        id,
			importKey: importKey,
			key:       keyVal,
			bpm:       bpmVal,
			length:    lengthVal,
			analyzed:  analyzedVal != 0,
		}
		return true, nil
	}
	var matched bool
	if stickID.EngineID > 0 {
		matched, err = readRow(`SELECT id, path, COALESCE(pdbImportKey, 0), COALESCE(key, -1), COALESCE(bpmAnalyzed, bpm, 0), COALESCE(length, 0), COALESCE(isAnalyzed, 0) FROM Track WHERE id = ? LIMIT 1`, stickID.EngineID)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return "", rbOnDemandTrackMeta{}, context.DeadlineExceeded
			}
			return "", rbOnDemandTrackMeta{}, bridge.ErrOnDemandUnavailable
		}
	}
	if !matched {
		matched, err = readRow(`SELECT id, path, COALESCE(pdbImportKey, 0), COALESCE(key, -1), COALESCE(bpmAnalyzed, bpm, 0), COALESCE(length, 0), COALESCE(isAnalyzed, 0) FROM Track WHERE path = ? OR path = ? ORDER BY id DESC LIMIT 1`, "../"+stickID.TrackRel, stickID.TrackRel)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return "", rbOnDemandTrackMeta{}, context.DeadlineExceeded
			}
			return "", rbOnDemandTrackMeta{}, bridge.ErrOnDemandUnavailable
		}
	}

	// Re-verify the DB inode/size/mtime did not change mid-read before caching.
	postDev, postIno, postSize, postModNS, ok := onDemandStatFile(mdb)
	if !ok || postDev != stickID.DBFile.Dev || postIno != stickID.DBFile.Ino {
		return "", rbOnDemandTrackMeta{}, bridge.ErrOnDemandUnavailable
	}
	if postSize == stickID.DBFile.Size && postModNS == stickID.DBFile.ModNS {
		s.rbOnDemandCacheMu.Lock()
		if s.rbOnDemandDBMeta == nil {
			s.rbOnDemandDBMeta = map[rbOnDemandDBKey]*rbOnDemandDBSnap{}
		}
		snap := s.rbOnDemandDBMeta[dbKey]
		if snap == nil {
			if len(s.rbOnDemandDBMeta) >= rbOnDemandDBMetaMax {
				var oldestKey rbOnDemandDBKey
				var oldestAt time.Time
				first := true
				for k, v := range s.rbOnDemandDBMeta {
					if first || v.at.Before(oldestAt) {
						oldestKey, oldestAt, first = k, v.at, false
					}
				}
				if !first {
					delete(s.rbOnDemandDBMeta, oldestKey)
				}
			}
			snap = &rbOnDemandDBSnap{uuid: liveUUID, tracks: map[string]rbOnDemandTrackMeta{}}
			s.rbOnDemandDBMeta[dbKey] = snap
		}
		if len(snap.tracks) >= rbOnDemandDBRowMax {
			clear(snap.tracks)
		}
		snap.uuid = liveUUID
		snap.tracks[stickID.TrackKey] = tm
		snap.at = time.Now()
		s.rbOnDemandCacheMu.Unlock()
	}
	return liveUUID, tm, nil
}

func (s *Server) resolveOnDemandIdentity(ctx context.Context, b *bridge.OnDemandBuilder, req bridge.OnDemandRequest, forPeer bool) (onDemandVerifiedID, bridge.OnDemandRequest, bool, error) {
	return s.resolveOnDemandIdentityWithImportKey(ctx, b, req, forPeer, 0)
}

func (s *Server) resolveOnDemandIdentityWithImportKey(ctx context.Context, b *bridge.OnDemandBuilder, req bridge.OnDemandRequest, forPeer bool, requireImportKey int64) (onDemandVerifiedID, bridge.OnDemandRequest, bool, error) {
	stickID, err := b.InspectIdentity(ctx, req)
	if err != nil {
		return onDemandVerifiedID{}, bridge.OnDemandRequest{}, false, err
	}
	liveUUID, tm, err := s.lookupOnDemandDBMeta(ctx, stickID)
	if err != nil {
		return onDemandVerifiedID{}, bridge.OnDemandRequest{}, false, err
	}
	if liveUUID == "" || stickID.SnapshotUUID == "" || liveUUID != stickID.SnapshotUUID {
		return onDemandVerifiedID{}, bridge.OnDemandRequest{}, false, bridge.ErrOnDemandUUIDMismatch
	}
	if req.UUID != "" && req.UUID != liveUUID {
		return onDemandVerifiedID{}, bridge.OnDemandRequest{}, false, bridge.ErrOnDemandUUIDMismatch
	}
	canonical := bridge.OnDemandRequest{
		Mount:      stickID.Mount,
		UUID:       liveUUID,
		EnginePath: "../" + stickID.TrackRel,
		EngineID:   req.EngineID,
		EngineKey:  req.EngineKey,
		EngineBPM:  req.EngineBPM,
		EngineLen:  req.EngineLen,
	}
	if !tm.found {
		if requireImportKey > 0 {
			return onDemandVerifiedID{}, bridge.OnDemandRequest{}, false, bridge.ErrOnDemandNotFound
		}
		if forPeer {
			// Defer peer answers until m.db holds the track row metadata needed to
			// produce the exact same canonical payload as the source deck's own call.
			return onDemandVerifiedID{}, bridge.OnDemandRequest{}, false, bridge.ErrOnDemandUnavailable
		}
		if canonical.EngineID <= 0 {
			return onDemandVerifiedID{}, bridge.OnDemandRequest{}, false, bridge.ErrOnDemandNotFound
		}
	} else {
		if canonical.EngineID > 0 && canonical.EngineID != tm.id {
			return onDemandVerifiedID{}, bridge.OnDemandRequest{}, false, bridge.ErrOnDemandNotFound
		}
		if requireImportKey > 0 && (tm.importKey <= 0 || tm.importKey != requireImportKey) {
			return onDemandVerifiedID{}, bridge.OnDemandRequest{}, false, bridge.ErrOnDemandNotFound
		}
		// Always key and build from the owner's own m.db Track row, never caller metadata.
		canonical.EngineID = tm.id
		canonical.EngineKey = tm.key
		canonical.EngineBPM = tm.bpm
		canonical.EngineLen = tm.length
	}
	id := onDemandVerifiedID{
		mount:         stickID.Mount,
		mountDev:      stickID.MountDev,
		mountMntID:    stickID.MountMntID,
		mountIno:      stickID.MountIno,
		snapshotUUID:  stickID.SnapshotUUID,
		liveUUID:      liveUUID,
		snapFile:      stickID.SnapFile,
		dbDev:         stickID.DBFile.Dev,
		dbIno:         stickID.DBFile.Ino,
		exportDB:      stickID.ExportDB,
		exportPDB:     stickID.ExportPDB,
		audioFile:     stickID.AudioFile,
		anlzFiles:     stickID.ANLZFiles,
		trackKey:      stickID.TrackKey,
		trackRel:      stickID.TrackRel,
		rowHash:       stickID.RowHash,
		engineID:      canonical.EngineID,
		importKey:     tm.importKey,
		engineKey:     canonical.EngineKey,
		engineBPMBits: math.Float64bits(canonical.EngineBPM),
		engineLen:     canonical.EngineLen,
	}
	return id, canonical, tm.analyzed, nil
}

// buildOnDemandCached builds one track's RBXA1 on-demand payload and shares the
// verified result across the source deck's own load and any shadow copies asking
// for it (peer-first or deck-first, concurrent single-flight or warm reuse).
func (s *Server) buildOnDemandCached(ctx context.Context, req bridge.OnDemandRequest, forPeer bool) (*bridge.OnDemandResult, error) {
	entry, err := s.buildOnDemandEntry(ctx, req, forPeer)
	if err != nil {
		return nil, err
	}
	return entry.res, nil
}

func (s *Server) buildOnDemandCachedWithImportKey(ctx context.Context, req bridge.OnDemandRequest, forPeer bool, requireImportKey int64) (*bridge.OnDemandResult, error) {
	entry, _, err := s.buildOnDemandEntryCheckedWithImportKey(ctx, req, forPeer, false, requireImportKey)
	if err != nil {
		return nil, err
	}
	return entry.res, nil
}

func (s *Server) buildOnDemandEntry(ctx context.Context, req bridge.OnDemandRequest, forPeer bool) (*rbOnDemandCacheEntry, error) {
	entry, _, err := s.buildOnDemandEntryCheckedWithImportKey(ctx, req, forPeer, false, 0)
	return entry, err
}

func (s *Server) buildOnDemandEntryChecked(ctx context.Context, req bridge.OnDemandRequest, forPeer, skipIfDBAnalyzed bool) (*rbOnDemandCacheEntry, bool, error) {
	return s.buildOnDemandEntryCheckedWithImportKey(ctx, req, forPeer, skipIfDBAnalyzed, 0)
}

func (s *Server) buildOnDemandEntryCheckedWithImportKey(ctx context.Context, req bridge.OnDemandRequest, forPeer, skipIfDBAnalyzed bool, requireImportKey int64) (*rbOnDemandCacheEntry, bool, error) {
	b := s.rbOnDemandBuilder()
	if b == nil {
		return nil, false, os.ErrNotExist
	}
	id, canonical, dbAnalyzed, err := s.resolveOnDemandIdentityWithImportKey(ctx, b, req, forPeer, requireImportKey)
	if err != nil {
		return nil, false, err
	}
	if skipIfDBAnalyzed && dbAnalyzed {
		return nil, true, nil
	}
	trackKey := onDemandCacheKey(id.mount, "../"+id.trackRel)
	now := time.Now()
	s.rbOnDemandCacheMu.Lock()
	if e := s.rbOnDemandCache[id]; e != nil {
		if now.Sub(e.at) < rbOnDemandCacheTTL {
			s.rbOnDemandCacheMu.Unlock()
			return e, dbAnalyzed, nil
		}
		delete(s.rbOnDemandCache, id)
	}
	if f := s.rbOnDemandFlights[id]; f != nil {
		f.waiters++
		s.rbOnDemandCacheMu.Unlock()
		res, werr := s.waitOnDemandFlight(ctx, f)
		if werr != nil {
			return nil, dbAnalyzed, werr
		}
		// Revalidate identity before authorizing a waiter/peer response.
		postID, _, _, verr := s.resolveOnDemandIdentityWithImportKey(ctx, b, req, forPeer, requireImportKey)
		if verr != nil || postID != id {
			return nil, dbAnalyzed, bridge.ErrOnDemandUnavailable
		}
		s.rbOnDemandCacheMu.Lock()
		e := s.rbOnDemandCache[id]
		s.rbOnDemandCacheMu.Unlock()
		if e != nil && e.res == res {
			return e, dbAnalyzed, nil
		}
		return &rbOnDemandCacheEntry{id: id, trackKey: trackKey, res: res, at: time.Now()}, dbAnalyzed, nil
	}
	buildCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	f := &rbOnDemandFlight{
		done:     make(chan struct{}),
		cancel:   cancel,
		waiters:  1,
		trackKey: trackKey,
	}
	if s.rbOnDemandFlights == nil {
		s.rbOnDemandFlights = map[onDemandVerifiedID]*rbOnDemandFlight{}
	}
	s.rbOnDemandFlights[id] = f
	s.rbOnDemandCacheMu.Unlock()
	// Yield any active background pass only after stick + row identity has been verified
	// and a cold build is actually starting.
	s.rbPriorityQueue().NoteLoading(now)

	go func() {
		defer cancel()
		s.rbOnDemandBuildCount.Add(1)
		res, berr := b.Build(buildCtx, canonical)
		if berr == nil && res != nil {
			postID, _, _, verr := s.resolveOnDemandIdentityWithImportKey(buildCtx, b, canonical, forPeer, requireImportKey)
			if verr != nil || postID != id {
				res, berr = nil, bridge.ErrOnDemandUnavailable
			}
		}
		s.rbOnDemandCacheMu.Lock()
		f.res, f.err = res, berr
		if berr == nil && res != nil && !f.evicted && s.rbOnDemandFlights[id] == f {
			if s.rbOnDemandCache == nil {
				s.rbOnDemandCache = map[onDemandVerifiedID]*rbOnDemandCacheEntry{}
			}
			if _, exists := s.rbOnDemandCache[id]; !exists && len(s.rbOnDemandCache) >= rbOnDemandCacheMax {
				var oldestKey onDemandVerifiedID
				var oldestAt time.Time
				first := true
				for k, v := range s.rbOnDemandCache {
					if first || v.at.Before(oldestAt) {
						oldestKey, oldestAt, first = k, v.at, false
					}
				}
				if !first {
					delete(s.rbOnDemandCache, oldestKey)
				}
			}
			s.rbOnDemandCache[id] = &rbOnDemandCacheEntry{
				id:       id,
				trackKey: trackKey,
				res:      res,
				at:       time.Now(),
			}
		}
		if s.rbOnDemandFlights[id] == f {
			delete(s.rbOnDemandFlights, id)
		}
		s.rbOnDemandCacheMu.Unlock()
		close(f.done)
	}()

	res, werr := s.waitOnDemandFlight(ctx, f)
	if werr != nil {
		return nil, dbAnalyzed, werr
	}
	postID, _, _, verr := s.resolveOnDemandIdentityWithImportKey(ctx, b, req, forPeer, requireImportKey)
	if verr != nil || postID != id {
		return nil, dbAnalyzed, bridge.ErrOnDemandUnavailable
	}
	s.rbOnDemandCacheMu.Lock()
	e := s.rbOnDemandCache[id]
	s.rbOnDemandCacheMu.Unlock()
	if e != nil && e.res == res {
		return e, dbAnalyzed, nil
	}
	return &rbOnDemandCacheEntry{id: id, trackKey: trackKey, res: res, at: time.Now()}, dbAnalyzed, nil
}

// onDemandEncodings decodes an entry's RBXA1 payload exactly once (even under
// concurrent first EXPF and native requests) and caches both native RBXA1 and
// EXPF v1 encodings alongside the precomputed generation hash.
func (s *Server) onDemandEncodings(e *rbOnDemandCacheEntry) (native, expf []byte, gen string, err error) {
	if e == nil || e.res == nil {
		return nil, nil, "", os.ErrNotExist
	}
	e.enc.once.Do(func() {
		s.rbOnDemandEncodeCount.Add(1)
		bpm := math.Float64frombits(e.id.engineBPMBits)
		dec, derr := rbstate.DecodePerf(e.res.RBXA1)
		if derr != nil {
			e.enc.err = derr
			return
		}
		p := enginedb.Performance{
			TrackData:   dec.TrackData,
			Overview:    dec.Overview,
			Beats:       dec.Beats,
			QuickCues:   dec.QuickCues,
			Loops:       dec.Loops,
			ActiveLoops: dec.ActiveLoops,
			Complete:    dec.Complete,
		}
		e.enc.native, e.enc.expf, e.enc.gen, e.enc.err = encodePerfPair(bpm, p)
	})
	return e.enc.native, e.enc.expf, e.enc.gen, e.enc.err
}

func (s *Server) hasWarmOnDemandEntry(trackKey string) bool {
	if trackKey == "" {
		return false
	}
	now := time.Now()
	s.rbOnDemandCacheMu.Lock()
	defer s.rbOnDemandCacheMu.Unlock()
	for _, e := range s.rbOnDemandCache {
		if e.trackKey == trackKey && now.Sub(e.at) < rbOnDemandCacheTTL {
			return true
		}
	}
	for _, f := range s.rbOnDemandFlights {
		if f.trackKey == trackKey {
			return true
		}
	}
	return false
}

// warmOnDemandCachedEntry returns a non-expired cached on-demand entry for
// trackKey without waiting on in-flight builds or triggering a cold build.
func (s *Server) warmOnDemandCachedEntry(trackKey string) *rbOnDemandCacheEntry {
	if trackKey == "" {
		return nil
	}
	now := time.Now()
	s.rbOnDemandCacheMu.Lock()
	defer s.rbOnDemandCacheMu.Unlock()
	for _, e := range s.rbOnDemandCache {
		if e.trackKey == trackKey && now.Sub(e.at) < rbOnDemandCacheTTL {
			return e
		}
	}
	return nil
}

// verifyWarmOnDemandGen verifies that cached entry e still matches the live
// stick/library/file identity for libPath and returns its precomputed
// generation digest without ever starting a cold on-demand build.
func (s *Server) verifyWarmOnDemandGen(ctx context.Context, libPath string, e *rbOnDemandCacheEntry) string {
	if e == nil || libPath == "" {
		return ""
	}
	b := s.rbOnDemandBuilder()
	if b == nil {
		return ""
	}
	mount, rel, ok := s.onDemandMountAndRel(libPath)
	if !ok {
		return ""
	}
	curID, _, _, err := s.resolveOnDemandIdentity(ctx, b, bridge.OnDemandRequest{
		Mount:      mount,
		EnginePath: "../" + rel,
		EngineKey:  -1,
	}, true)
	if err != nil || curID != e.id {
		return ""
	}
	_, _, gen, err := s.onDemandEncodings(e)
	if err != nil {
		return ""
	}
	return gen
}

func (s *Server) waitOnDemandFlight(ctx context.Context, f *rbOnDemandFlight) (*bridge.OnDemandResult, error) {
	select {
	case <-f.done:
		return f.res, f.err
	case <-ctx.Done():
		s.rbOnDemandCacheMu.Lock()
		f.waiters--
		if f.waiters == 0 && f.cancel != nil {
			f.cancel()
		}
		s.rbOnDemandCacheMu.Unlock()
		return nil, ctx.Err()
	}
}

// evictOnDemandCacheForTrackKey drops any cached on-demand payload and cancels
// any in-flight build for a canonical mount+folded-path key without retaining
// unbounded per-path history.
func (s *Server) evictOnDemandCacheForTrackKey(trackKey string) {
	if trackKey == "" {
		return
	}
	s.rbOnDemandCacheMu.Lock()
	for id, e := range s.rbOnDemandCache {
		if e.trackKey == trackKey {
			delete(s.rbOnDemandCache, id)
		}
	}
	for id, f := range s.rbOnDemandFlights {
		if f.trackKey == trackKey {
			f.evicted = true
			if f.cancel != nil {
				f.cancel()
			}
			delete(s.rbOnDemandFlights, id)
		}
	}
	s.rbOnDemandCacheMu.Unlock()
}

// evictOnDemandCacheForPath drops cached on-demand state for a verified media path.
// It never guesses an inMusicTempFiles origin by basename and size; callers pass
// the deck's verified libraryPath or canonical track key.
func (s *Server) evictOnDemandCacheForPath(srcPath string) {
	s.evictOnDemandCacheForTrackKey(s.canonicalOnDemandTrackKey(srcPath))
}

func writeOnDemandError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		w.Header().Set("X-RB-Status", "timeout")
		fail(w, http.StatusGatewayTimeout, err)
	case errors.Is(err, bridge.ErrOnDemandNotFound):
		w.Header().Set("X-RB-Status", "not_found")
		fail(w, http.StatusNotFound, err)
	case errors.Is(err, bridge.ErrOnDemandUUIDMismatch):
		w.Header().Set("X-RB-Status", "uuid_mismatch")
		fail(w, http.StatusConflict, err)
	case errors.Is(err, errLinkFillNotCommitted):
		w.Header().Set("X-RB-Status", "not_committed")
		fail(w, http.StatusConflict, err)
	case errors.Is(err, errOnDemandNotRepairable):
		w.Header().Set("X-RB-Status", "not_repairable")
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
}

func decodeRepairHex(raw string, maxBytes int) ([]byte, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	if len(raw) > maxBytes*2 {
		return nil, errOnDemandNotRepairable
	}
	b, err := hex.DecodeString(raw)
	if err != nil {
		return nil, errOnDemandNotRepairable
	}
	return b, nil
}

// filledJournalMatchesRow reports whether /data/enginex/rb-stage/<uuid>/filled.jsonl already
// recorded this row's current (beats, cues, loops) digests as filled by EngineX.
func (s *Server) filledJournalMatchesRow(ctx context.Context, uuid string, engineID int64, beats, cues, loops []byte) bool {
	if !validRBStageUUID(uuid) || engineID <= 0 || len(beats) == 0 {
		return false
	}
	root, err := s.rbStageRoot()
	if err != nil {
		return false
	}
	f, err := os.Open(filepath.Join(root, uuid, "filled.jsonl"))
	if err != nil {
		return false
	}
	defer f.Close()
	norm := base64.StdEncoding.EncodeToString(enginedb.WrittenDigest(beats, cues, loops))
	raw := base64.StdEncoding.EncodeToString(enginedb.RawWrittenDigest(beats, cues, loops))
	noMain := base64.StdEncoding.EncodeToString(linkWrittenDigestNoMain(beats, cues, loops))
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 8<<10), 256<<10)
	n := 0
	for sc.Scan() {
		n++
		if n&63 == 0 && ctx.Err() != nil {
			return false
		}
		var ln rbFilledJournalLine
		if json.Unmarshal(sc.Bytes(), &ln) != nil || ln.ID != engineID {
			continue
		}
		if ln.WR != "" && (ln.WR == norm || ln.WR == raw) {
			return true
		}
		if ln.WRNM != "" && ln.WRNM == noMain {
			return true
		}
	}
	return false
}

// verifyRepairCandidate inspects the live m.db row on the stick holder and verifies that it is
// an unedited stock-Engine-overwritten rekordbox row (never DJ-edited, never filled by us,
// never a stock Engine track) and matches the exact blobs observed by the caller hook.
func (s *Server) verifyRepairCandidate(ctx context.Context, mount, libUUID, enginePath string, engineID, requireImportKey int64, curBeatsHex, curCuesHex, curLoopsHex string) error {
	curBeats, err := decodeRepairHex(curBeatsHex, 256)
	if err != nil {
		return err
	}
	curCues, err := decodeRepairHex(curCuesHex, 512)
	if err != nil {
		return err
	}
	curLoops, err := decodeRepairHex(curLoopsHex, 768)
	if err != nil {
		return err
	}
	if len(curBeats) == 0 && len(curCues) == 0 && len(curLoops) == 0 {
		return errOnDemandNotRepairable
	}
	b := s.rbOnDemandBuilder()
	if b == nil {
		return bridge.ErrOnDemandUnavailable
	}
	stickID, err := b.InspectIdentity(ctx, bridge.OnDemandRequest{
		Mount:      mount,
		UUID:       libUUID,
		EnginePath: enginePath,
		EngineID:   engineID,
		EngineKey:  -1,
	})
	if err != nil {
		return err
	}

	mdb := filepath.Join(stickID.Mount, "Engine Library", "Database2", "m.db")
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: mdb}).String()+"?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(500)")
	if err != nil {
		return bridge.ErrOnDemandUnavailable
	}
	defer db.Close()

	var liveUUID string
	if err := db.QueryRowContext(ctx, `SELECT coalesce(uuid, '') FROM Information LIMIT 1`).Scan(&liveUUID); err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return context.DeadlineExceeded
		}
		if errors.Is(err, sql.ErrNoRows) {
			return bridge.ErrOnDemandUUIDMismatch
		}
		return bridge.ErrOnDemandUnavailable
	}
	liveUUID = strings.TrimSpace(liveUUID)
	if liveUUID == "" || liveUUID != stickID.SnapshotUUID || (libUUID != "" && liveUUID != libUUID) {
		return bridge.ErrOnDemandUUIDMismatch
	}

	var (
		rowID, importKey int64
		rowPath          string
		lockedVal        int
		row              enginedb.StockOverwrittenRow
	)
	queryByID := `
		SELECT t.id, t.path, COALESCE(t.pdbImportKey, 0), COALESCE(t.isBeatGridLocked, 0),
		       p.trackData, p.overviewWaveFormData, p.beatData, p.quickCues, p.loops,
		       COALESCE(p.activeOnLoadLoops, 0), p.thirdPartySourceId
		FROM Track t
		JOIN PerformanceData p ON p.trackId = t.id
		WHERE t.id = ? LIMIT 1`
	queryByPath := `
		SELECT t.id, t.path, COALESCE(t.pdbImportKey, 0), COALESCE(t.isBeatGridLocked, 0),
		       p.trackData, p.overviewWaveFormData, p.beatData, p.quickCues, p.loops,
		       COALESCE(p.activeOnLoadLoops, 0), p.thirdPartySourceId
		FROM Track t
		JOIN PerformanceData p ON p.trackId = t.id
		WHERE t.path = ? OR t.path = ? ORDER BY t.id DESC LIMIT 1`
	var qerr error
	if engineID > 0 {
		qerr = db.QueryRowContext(ctx, queryByID, engineID).Scan(
			&rowID, &rowPath, &importKey, &lockedVal,
			&row.TrackData, &row.Overview, &row.Beats, &row.QuickCues, &row.Loops,
			&row.ActiveOnLoadLoops, &row.ThirdPartySourceID,
		)
	} else {
		qerr = db.QueryRowContext(ctx, queryByPath, "../"+stickID.TrackRel, stickID.TrackRel).Scan(
			&rowID, &rowPath, &importKey, &lockedVal,
			&row.TrackData, &row.Overview, &row.Beats, &row.QuickCues, &row.Loops,
			&row.ActiveOnLoadLoops, &row.ThirdPartySourceID,
		)
	}
	if qerr != nil {
		if errors.Is(qerr, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return context.DeadlineExceeded
		}
		if errors.Is(qerr, sql.ErrNoRows) {
			return bridge.ErrOnDemandNotFound
		}
		return bridge.ErrOnDemandUnavailable
	}
	cleanRow := strings.TrimPrefix(strings.ReplaceAll(strings.TrimSpace(rowPath), "\\", "/"), "../")
	cleanRow = strings.TrimPrefix(path.Clean("/"+cleanRow), "/")
	if rbstate.FoldPath(cleanRow) != stickID.TrackKey {
		return bridge.ErrOnDemandNotFound
	}
	if importKey <= 0 || (requireImportKey > 0 && importKey != requireImportKey) {
		return bridge.ErrOnDemandNotFound
	}
	row.BeatGridLocked = lockedVal != 0
	row.FastKnown = stickID.FastKnown
	row.FastCues = stickID.FastCues
	row.FastLoops = stickID.FastLoops
	row.FastActive = stickID.FastActive
	if !enginedb.IsStockOverwrittenPerformance(row) {
		return errOnDemandNotRepairable
	}
	// Unconditionally require the caller hook's observed blobs to match m.db byte-for-byte
	// so stale or concurrent edits can never slip through.
	if !bytes.Equal(row.Beats, curBeats) || !bytes.Equal(row.QuickCues, curCues) || !bytes.Equal(row.Loops, curLoops) {
		return errOnDemandNotRepairable
	}
	if s.filledJournalMatchesRow(ctx, liveUUID, rowID, row.Beats, row.QuickCues, row.Loops) {
		return errOnDemandNotRepairable
	}
	return nil
}

func rbxa1FirstBPM(rbxa1 []byte) float64 {
	dec, err := rbstate.DecodePerf(rbxa1)
	if err != nil || !dec.Complete || len(dec.Beats) == 0 {
		return 0
	}
	return enginedb.BeatDataFirstBPM(dec.Beats)
}

func writeOnDemandRBXA1(w http.ResponseWriter, res *bridge.OnDemandResult, libUUID, ownLink string) {
	writeOnDemandRBXA1WithRepair(w, res, libUUID, ownLink, false)
}

func writeOnDemandRBXA1WithRepair(w http.ResponseWriter, res *bridge.OnDemandResult, libUUID, ownLink string, repair bool) {
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(res.RBXA1)))
	if libUUID != "" {
		w.Header().Set("X-RB-UUID", libUUID)
	}
	if ownLink != "" {
		w.Header().Set("X-RB-Owner", ownLink)
	}
	if repair {
		w.Header().Set("X-RB-Repair", "1")
		if bpm := rbxa1FirstBPM(res.RBXA1); bpm > 0 {
			w.Header().Set("X-RB-BPM", fmt.Sprintf("%.4f", bpm))
		}
	}
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
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if strings.TrimSpace(req.Path) == "" {
		fail(w, http.StatusBadRequest, errors.New("missing path"))
		return
	}
	if strings.TrimSpace(req.Mount) == "" && strings.HasPrefix(strings.TrimSpace(req.DB), "net://") {
		s.serveRekordboxLinkRelay(w, r, req)
		return
	}
	mount, ok := resolveOnDemandMount(req.Mount, req.DB, s.onDemandMediaRoot())
	if !ok {
		w.Header().Set("X-RB-Status", "not_found")
		fail(w, http.StatusNotFound, errors.New("volume not found"))
		return
	}

	if req.Repair {
		if err := s.verifyRepairCandidate(r.Context(), mount, strings.TrimSpace(req.UUID), req.Path, req.ID, req.ImportKey, req.CurBeats, req.CurCues, req.CurLoops); err != nil {
			writeOnDemandError(w, err)
			return
		}
	}

	engineKey := -1
	if req.Key != nil {
		engineKey = *req.Key
	}
	res, err := s.buildOnDemandCached(r.Context(), bridge.OnDemandRequest{
		Mount:      mount,
		UUID:       strings.TrimSpace(req.UUID),
		EnginePath: req.Path,
		EngineID:   req.ID,
		EngineKey:  engineKey,
		EngineBPM:  req.BPM,
		EngineLen:  req.Length,
	}, false)
	if err != nil {
		writeOnDemandError(w, err)
		return
	}
	writeOnDemandRBXA1WithRepair(w, res, strings.TrimSpace(req.UUID), "", req.Repair)
}
