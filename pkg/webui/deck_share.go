package webui

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/providers"
)

const (
	deckShareGrantTTL  = 30 * time.Second
	deckShareMaxGrants = 8
	deckShareMaxBundle = 2 << 20 // 2 MiB
	deckShareUndoFile  = "deck-share-undo.json"
)

type deckShareDeezer struct {
	UserID string `json:"userId,omitempty"`
	ARL    string `json:"arl,omitempty"`
}

type deckShareNetease struct {
	UserID int64  `json:"userId"`
	Cookie string `json:"cookie"`
}

type deckShareSpotify struct {
	Token    *providers.Token `json:"token,omitempty"`
	ClientID string           `json:"clientId,omitempty"`
	Links    json.RawMessage  `json:"links,omitempty"`
}

type deckShareAMHelper struct {
	providers.AppleMusicHelperConfig
	TokenCleared bool `json:"tokenCleared,omitempty"`
}

type deckShareAppleMusic struct {
	Owner  *providers.AppleMusicOwner `json:"owner,omitempty"`
	Helper *deckShareAMHelper         `json:"helper,omitempty"`
}

type deckShareSoundCloud struct {
	UserID   int64  `json:"userId,omitempty"`
	Username string `json:"username,omitempty"`
	ClientID string `json:"clientId,omitempty"`
	Secret   string `json:"secret,omitempty"`
	Token    string `json:"token,omitempty"`
}

type deckShareServers struct {
	Slot   *providers.NASConfig `json:"slot,omitempty"`
	Name   string               `json:"name,omitempty"`
	WebDAV []savedServer        `json:"webdav,omitempty"`
	SMB    []savedServer        `json:"smb,omitempty"`
}

type deckShareBundle struct {
	Version    int                  `json:"version"`
	Board      string               `json:"board,omitempty"`
	Deezer     *deckShareDeezer     `json:"deezer,omitempty"`
	Netease    *deckShareNetease    `json:"netease,omitempty"`
	Spotify    *deckShareSpotify    `json:"spotify,omitempty"`
	AppleMusic *deckShareAppleMusic `json:"applemusic,omitempty"`
	SoundCloud *deckShareSoundCloud `json:"soundcloud,omitempty"`
	WebDAVOn   *bool                `json:"webdavOn,omitempty"`
	NASOn      *bool                `json:"nasOn,omitempty"`
	Servers    *deckShareServers    `json:"servers,omitempty"`
	Skipped    []string             `json:"skipped,omitempty"`
}

// deckShareSummary carries non-secret display names only (never tokens, URLs, or passwords).
type deckShareSummary struct {
	Peer    string   `json:"peer"`
	Board   string   `json:"board,omitempty"`
	Copied  []string `json:"copied"`
	WebDAV  []string `json:"webdav,omitempty"`
	NAS     []string `json:"nas,omitempty"`
	Skipped []string `json:"skipped,omitempty"`
}

type deckShareGrant struct {
	Token   string
	Peer    string
	Expires time.Time
	Bundle  deckShareBundle
}

type deckShareUndo struct {
	Files    map[string]copyFileSnapshot `json:"files"`
	LastCopy deckShareSummary            `json:"lastCopy"`
}

func (s *Server) spotifyTokenFilePath() string {
	if s.cfg.SpotifyTokenPath != "" {
		return s.cfg.SpotifyTokenPath
	}
	if s.cfg.StateDir == "" {
		return ""
	}
	return filepath.Join(s.cfg.StateDir, "spotify-token.json")
}

func isLoopbackURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Hostname() == "" {
		return false
	}
	h := strings.ToLower(strings.Trim(u.Hostname(), "[]"))
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	if ip := net.ParseIP(h); ip != nil && ip.IsLoopback() {
		return true
	}
	return false
}

// exportDeckShareBundle snapshots Deck A's shareable logins and servers.
// Single-use rotating refresh tokens (Beatport, real TIDAL, SoundCloud OAuth user
// refresh) and device-bound Spotify librespot credentials are never exported.
func (s *Server) exportDeckShareBundle() (deckShareBundle, deckShareSummary, error) {
	b := deckShareBundle{Version: 1, Board: s.board}
	var sum deckShareSummary
	sum.Board = s.board
	sum.Copied = []string{}

	if s.cfg.StateDir != "" {
		// 1. Deezer
		if dz := s.savedDeezerOwner(); dz.UserID != "" || dz.ARL != "" {
			b.Deezer = &deckShareDeezer{UserID: dz.UserID, ARL: dz.ARL}
			sum.Copied = append(sum.Copied, "deezer")
		}

		// 2. NetEase
		if raw, err := os.ReadFile(filepath.Join(s.cfg.StateDir, "netease-owner.json")); err == nil {
			var ne struct {
				Cookie string `json:"Cookie"`
				UserID int64  `json:"UserID"`
			}
			if json.Unmarshal(raw, &ne) == nil && ne.Cookie != "" && ne.UserID > 0 {
				b.Netease = &deckShareNetease{UserID: ne.UserID, Cookie: ne.Cookie}
				sum.Copied = append(sum.Copied, "netease")
			}
		}

		// 3. Spotify (OAuth token + optional custom client ID + public links; never librespot/device-id)
		var sp deckShareSpotify
		if spPath := s.spotifyTokenFilePath(); spPath != "" {
			if raw, err := os.ReadFile(spPath); err == nil {
				var tok providers.Token
				if json.Unmarshal(raw, &tok) == nil && (tok.AccessToken != "" || tok.RefreshToken != "") {
					sp.Token = &tok
				}
			}
		}
		if raw, err := os.ReadFile(s.spotifyClientPath()); err == nil {
			if cid := strings.TrimSpace(string(raw)); spotifyClientIDRe.MatchString(cid) {
				sp.ClientID = cid
			}
		}
		if raw, err := os.ReadFile(filepath.Join(s.cfg.StateDir, "spotify-links.json")); err == nil {
			var links []json.RawMessage
			if json.Unmarshal(raw, &links) == nil && len(links) > 0 {
				sp.Links = json.RawMessage(bytes.TrimSpace(raw))
			}
		}
		if sp.Token != nil || len(sp.Links) > 0 {
			b.Spotify = &sp
			sum.Copied = append(sum.Copied, "spotify")
		} else if _, err := os.Stat(filepath.Join(s.cfg.StateDir, "spotify-librespot.json")); err == nil {
			b.Skipped = append(b.Skipped, "spotify (device-bound librespot login cannot move without OAuth)")
		}

		// 4. Apple Music (EngineX owner + reachable non-loopback helper)
		var am deckShareAppleMusic
		if s.appleMusic != nil {
			owner := s.appleMusic.Owner()
			if owner.UserToken != "" || len(owner.Links) > 0 {
				am.Owner = &owner
			}
			cfg := s.appleMusic.HelperConfig()
			if cfg.URL != "" && !isLoopbackURL(cfg.URL) {
				am.Helper = &deckShareAMHelper{AppleMusicHelperConfig: cfg, TokenCleared: s.appleMusicHelperTokenCleared}
			}
		}
		if am.Owner != nil || am.Helper != nil {
			b.AppleMusic = &am
			sum.Copied = append(sum.Copied, "applemusic")
		}

		// 5. SoundCloud (safe subset: profile + app client ID/secret + static token; never rotating user refresh)
		var scOwner soundCloudOwner
		if raw, err := os.ReadFile(s.soundCloudOwnerPath()); err == nil {
			_ = json.Unmarshal(raw, &scOwner)
		}
		if s.cfg.BPM != nil {
			keys := s.cfg.BPM.Config()
			if scOwner.ClientID == "" {
				scOwner.ClientID = keys.SoundCloudClientID
			}
			if scOwner.Secret == "" {
				scOwner.Secret = keys.SoundCloudSecret
			}
			if scOwner.Token == "" {
				scOwner.Token = keys.SoundCloudToken
			}
		}
		if scOwner.UserID != 0 || scOwner.Username != "" || scOwner.ClientID != "" || scOwner.Secret != "" || scOwner.Token != "" {
			b.SoundCloud = &deckShareSoundCloud{
				UserID:   scOwner.UserID,
				Username: scOwner.Username,
				ClientID: scOwner.ClientID,
				Secret:   scOwner.Secret,
				Token:    scOwner.Token,
			}
			sum.Copied = append(sum.Copied, "soundcloud")
		}
		if s.soundCloudAuth != nil && s.soundCloudAuth.HasUser() && scOwner.Token == "" {
			b.Skipped = append(b.Skipped, "soundcloud oauth token (single-use rotating refresh token)")
		}
	}

	// 6. Services that cannot be shared safely
	if s.beatport != nil && s.beatport.Ready() {
		b.Skipped = append(b.Skipped, "beatport (single-use rotating refresh token)")
	}
	if s.tidal != nil && s.tidal.hasRealAuth() {
		b.Skipped = append(b.Skipped, "tidal (single-use rotating refresh token)")
	}

	// 7. WebDAV and NAS servers (nas.json)
	s.uiMu.Lock()
	ui := s.loadUI()
	nf, nasErr := s.readNASFileLocked()
	s.uiMu.Unlock()
	if nasErr == nil {
		wdOn := serversOn(kindWebDAV, ui, nf)
		nasOn := serversOn(kindNAS, ui, nf)
		var srv deckShareServers
		hasSrv := false
		if slotNamed(nf.NASConfig) && nasConfigReady(nf.NASConfig) {
			includeSlot := true
			if k, ok := kindOfProvider(nf.Protocol); ok {
				includeSlot = (k == kindWebDAV && wdOn) || (k == kindNAS && nasOn)
			}
			if includeSlot {
				cfgCopy := nf.NASConfig
				srv.Slot = &cfgCopy
				srv.Name = nf.Name
				hasSrv = true
				name := strings.TrimSpace(nf.Name)
				if name == "" {
					name = serverHost(nf.URL)
				}
				if nf.Protocol == webdavScope {
					sum.WebDAV = append(sum.WebDAV, name)
				} else {
					sum.NAS = append(sum.NAS, name)
				}
			}
		}
		if wdOn {
			for _, v := range nf.WebDAV {
				cfg := providers.NASConfig{Protocol: "webdav", URL: v.URL, Username: v.Username, Password: v.Password, Enabled: v.Enabled}
				if nasConfigReady(cfg) {
					srv.WebDAV = append(srv.WebDAV, v)
					hasSrv = true
					name := strings.TrimSpace(v.Name)
					if name == "" {
						name = serverHost(v.URL)
					}
					sum.WebDAV = append(sum.WebDAV, name)
				}
			}
		}
		if nasOn {
			for _, v := range nf.SMB {
				cfg := providers.NASConfig{Protocol: "smb", URL: v.URL, Username: v.Username, Password: v.Password, Enabled: v.Enabled}
				if nasConfigReady(cfg) {
					srv.SMB = append(srv.SMB, v)
					hasSrv = true
					name := strings.TrimSpace(v.Name)
					if name == "" {
						name = serverHost(v.URL)
					}
					sum.NAS = append(sum.NAS, name)
				}
			}
		}
		if hasSrv {
			b.Servers = &srv
			b.WebDAVOn = &wdOn
			b.NASOn = &nasOn
		}
	}

	sum.Skipped = slices.Clone(b.Skipped)
	return b, sum, nil
}

// ---- Source endpoints on Deck A ----

func (s *Server) handlePeerDeckShareRequest(w http.ResponseWriter, r *http.Request) {
	if !isLANPeerRequest(r) {
		fail(w, http.StatusForbidden, errors.New("LAN peers only"))
		return
	}
	var in struct {
		Code  string `json:"code"`
		Token string `json:"token"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil || dec.Decode(new(any)) != io.EOF {
		fail(w, http.StatusBadRequest, errors.New("invalid deck share request"))
		return
	}
	if !isHexString(in.Token, 32) {
		fail(w, http.StatusBadRequest, errors.New("invalid transfer token"))
		return
	}
	if normalizeAccessCode(in.Code) == "" {
		fail(w, http.StatusBadRequest, errors.New("access code required"))
		return
	}
	ok, locked := s.tryCode(r, in.Code)
	if locked {
		fail(w, http.StatusTooManyRequests, errAuthLocked)
		return
	}
	if !ok {
		fail(w, http.StatusUnauthorized, errors.New("invalid access code"))
		return
	}
	peer := copyRemoteIP(r)
	if peer == "" {
		fail(w, http.StatusBadRequest, errors.New("unknown peer address"))
		return
	}
	bundle, summary, err := s.exportDeckShareBundle()
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	now := time.Now()
	s.shareMu.Lock()
	if s.shareGrants == nil {
		s.shareGrants = map[string]deckShareGrant{}
	}
	for tok, g := range s.shareGrants {
		if now.After(g.Expires) {
			delete(s.shareGrants, tok)
		}
	}
	if len(s.shareGrants) >= deckShareMaxGrants {
		s.shareMu.Unlock()
		fail(w, http.StatusConflict, errors.New("another deck share transfer is pending"))
		return
	}
	s.shareGrants[in.Token] = deckShareGrant{
		Token:   in.Token,
		Peer:    peer,
		Expires: now.Add(deckShareGrantTTL),
		Bundle:  bundle,
	}
	s.shareMu.Unlock()
	respond(w, map[string]any{"ok": true, "summary": summary})
}

func (s *Server) handlePeerDeckShareTransfer(w http.ResponseWriter, r *http.Request) {
	if !isLANPeerRequest(r) {
		fail(w, http.StatusForbidden, errors.New("LAN peers only"))
		return
	}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "DeckShare ")
	peer := copyRemoteIP(r)
	now := time.Now()

	s.shareMu.Lock()
	var matched *deckShareGrant
	for tok, g := range s.shareGrants {
		if now.After(g.Expires) {
			delete(s.shareGrants, tok)
			continue
		}
		if g.Peer == peer && subtle.ConstantTimeCompare([]byte(token), []byte(g.Token)) == 1 {
			cp := g
			matched = &cp
			delete(s.shareGrants, tok) // single-use
			break
		}
	}
	s.shareMu.Unlock()

	if matched == nil {
		fail(w, http.StatusUnauthorized, errors.New("deck share transfer grant required"))
		return
	}
	respond(w, matched.Bundle)
}

// ---- Receiver endpoints & apply/undo on Deck B ----

func (s *Server) deckShareUndoPath() string {
	return filepath.Join(s.copyDir(), deckShareUndoFile)
}

func (s *Server) deckShareFilePaths() []string {
	var paths []string
	if s.cfg.StateDir != "" {
		paths = append(paths,
			filepath.Join(s.cfg.StateDir, "deezer-owner.json"),
			filepath.Join(s.cfg.StateDir, "netease-owner.json"),
			filepath.Join(s.cfg.StateDir, "spotify-client-id"),
			filepath.Join(s.cfg.StateDir, "spotify-links.json"),
			filepath.Join(s.cfg.StateDir, "applemusic-owner.json"),
			filepath.Join(s.cfg.StateDir, "applemusic-owner.json.forgot"),
			filepath.Join(s.cfg.StateDir, "applemusic-helper.json"),
			filepath.Join(s.cfg.StateDir, "soundcloud-owner.json"),
			filepath.Join(s.cfg.StateDir, "soundcloud-token.json"),
		)
	}
	if spPath := s.spotifyTokenFilePath(); spPath != "" && !slices.Contains(paths, spPath) {
		paths = append(paths, spPath)
	}
	if s.cfg.BPM != nil && s.cfg.BPM.ConfigPath != "" {
		paths = append(paths, s.cfg.BPM.ConfigPath)
	}
	paths = append(paths, s.nasConfigPath(), filepath.Join(s.uiDir(), "settings.conf"))
	return paths
}

func (s *Server) validateDeckShareBundle(b deckShareBundle) error {
	if b.Version != 1 {
		return errors.New("unsupported deck share bundle version")
	}
	if b.Deezer != nil && b.Deezer.UserID == "" && b.Deezer.ARL == "" {
		return errors.New("invalid deezer login in bundle")
	}
	if b.Netease != nil && (b.Netease.Cookie == "" || b.Netease.UserID <= 0) {
		return errors.New("invalid netease login in bundle")
	}
	if b.Spotify != nil {
		if b.Spotify.ClientID != "" && !spotifyClientIDRe.MatchString(b.Spotify.ClientID) {
			return errors.New("invalid spotify client ID in bundle")
		}
		if b.Spotify.Token != nil && b.Spotify.Token.AccessToken == "" && b.Spotify.Token.RefreshToken == "" {
			return errors.New("invalid spotify token in bundle")
		}
		if len(b.Spotify.Links) > 0 && !json.Valid(b.Spotify.Links) {
			return errors.New("invalid spotify links in bundle")
		}
	}
	if b.AppleMusic != nil {
		if b.AppleMusic.Owner != nil {
			sf := strings.ToLower(strings.TrimSpace(b.AppleMusic.Owner.Storefront))
			if sf != "" && (len(sf) != 2 || sf[0] < 'a' || sf[0] > 'z' || sf[1] < 'a' || sf[1] > 'z') {
				return errors.New("invalid apple music storefront in bundle")
			}
			if b.AppleMusic.Owner.UserToken == "" && len(b.AppleMusic.Owner.Links) == 0 {
				return errors.New("invalid apple music owner in bundle")
			}
		}
		if b.AppleMusic.Helper != nil {
			if !validAppleMusicHelperURL(b.AppleMusic.Helper.URL) || !validAppleMusicHelperURL(b.AppleMusic.Helper.LoginURL) {
				return errors.New("invalid apple music helper URL in bundle")
			}
		}
	}
	if b.Servers != nil {
		if b.Servers.Slot != nil && b.Servers.Slot.URL != "" {
			p := strings.ToLower(strings.TrimSpace(b.Servers.Slot.Protocol))
			if !isNASProvider(p) || validateNASURL(b.Servers.Slot.URL) != nil {
				return errors.New("invalid slot server URL in bundle")
			}
		}
		for _, v := range b.Servers.WebDAV {
			if validServerURL(kindWebDAV, v.URL) != nil {
				return errors.New("invalid WebDAV server URL in bundle")
			}
		}
		for _, v := range b.Servers.SMB {
			if validServerURL(kindNAS, v.URL) != nil {
				return errors.New("invalid NAS server URL in bundle")
			}
		}
	}
	return nil
}

func (s *Server) snapshotDeckShareFiles() (map[string]copyFileSnapshot, error) {
	snaps := map[string]copyFileSnapshot{}
	for _, p := range s.deckShareFilePaths() {
		snap, err := snapshotCopyFile(p)
		if err != nil {
			return nil, err
		}
		snaps[p] = snap
	}
	return snaps, nil
}

func (s *Server) restoreDeckShareFiles(snaps map[string]copyFileSnapshot) error {
	var errs []error
	for _, p := range s.deckShareFilePaths() {
		snap, ok := snaps[p]
		if !ok {
			continue
		}
		if err := restoreCopyFile(p, snap); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (s *Server) readDeckShareUndo() (deckShareUndo, bool) {
	var u deckShareUndo
	b, err := os.ReadFile(s.deckShareUndoPath())
	if err != nil || json.Unmarshal(b, &u) != nil || len(u.Files) == 0 {
		return deckShareUndo{}, false
	}
	return u, true
}

func (s *Server) deckShareStatus(w http.ResponseWriter, r *http.Request) {
	s.probeDeckSharePeers()
	var lastCopy *deckShareSummary
	hasUndo := false
	if u, ok := s.readDeckShareUndo(); ok {
		hasUndo = true
		cp := u.LastCopy
		lastCopy = &cp
	}
	respond(w, map[string]any{
		"peers":    s.sharePeers(),
		"undo":     hasUndo,
		"lastCopy": lastCopy,
	})
}

func (s *Server) deckShareControl(w http.ResponseWriter, r *http.Request) {
	if origin := r.Header.Get("Origin"); origin != "" && origin != "null" {
		u, err := url.Parse(origin)
		if err != nil || u.Host != r.Host {
			fail(w, http.StatusForbidden, errors.New("same-origin request required"))
			return
		}
	}
	var in struct {
		Action string `json:"action"`
		Peer   string `json:"peer"`
		Code   string `json:"code"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16384)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil || dec.Decode(new(any)) != io.EOF {
		fail(w, http.StatusBadRequest, errors.New("invalid deck share request"))
		return
	}
	switch in.Action {
	case "pull":
		summary, status, err := s.pullDeckShare(in.Peer, in.Code)
		if err != nil {
			fail(w, status, err)
			return
		}
		respond(w, map[string]any{"ok": true, "summary": summary, "undo": true})
	case "undo":
		if err := s.undoDeckShare(); err != nil {
			fail(w, http.StatusConflict, err)
			return
		}
		respond(w, map[string]any{"ok": true, "restored": true, "undo": false})
	default:
		fail(w, http.StatusBadRequest, errors.New("unknown deck share action"))
	}
}

func (s *Server) pullDeckShare(peer, code string) (deckShareSummary, int, error) {
	peer = strings.TrimSpace(peer)
	peers := s.sharePeers()
	if peer == "" && len(peers) == 1 {
		peer = peers[0]
	}
	if peer == "" || !slices.Contains(peers, peer) {
		return deckShareSummary{}, http.StatusBadRequest, errors.New("select a discovered EngineX peer deck")
	}
	if normalizeAccessCode(code) == "" {
		return deckShareSummary{}, http.StatusBadRequest, errors.New("enter the other deck's access code")
	}

	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return deckShareSummary{}, http.StatusInternalServerError, err
	}
	token := hex.EncodeToString(nonce[:])

	reqBody, _ := json.Marshal(map[string]string{"code": code, "token": token})
	reqResp, err := copyPeerHTTP.Post(s.sharePeerURL(peer, "/peer/deck-share/request"), "application/json", bytes.NewReader(reqBody))
	if err != nil {
		return deckShareSummary{}, http.StatusBadGateway, errors.New("could not reach source deck")
	}
	defer reqResp.Body.Close()
	if reqResp.StatusCode != http.StatusOK {
		var errWire struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(reqResp.Body, 4096)).Decode(&errWire)
		msg := errWire.Error
		if msg == "" {
			msg = fmt.Sprintf("source deck refused code (%d)", reqResp.StatusCode)
		}
		return deckShareSummary{}, reqResp.StatusCode, errors.New(msg)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(reqResp.Body, 64<<10))

	tReq, err := http.NewRequest(http.MethodGet, s.sharePeerURL(peer, "/peer/deck-share/transfer"), nil)
	if err != nil {
		return deckShareSummary{}, http.StatusInternalServerError, err
	}
	tReq.Header.Set("Authorization", "DeckShare "+token)
	tResp, err := copyPeerHTTP.Do(tReq)
	if err != nil {
		return deckShareSummary{}, http.StatusBadGateway, errors.New("could not fetch shared logins from source deck")
	}
	defer tResp.Body.Close()
	if tResp.StatusCode != http.StatusOK {
		return deckShareSummary{}, tResp.StatusCode, fmt.Errorf("deck share transfer refused (%d)", tResp.StatusCode)
	}

	var bundle deckShareBundle
	dec := json.NewDecoder(io.LimitReader(tResp.Body, deckShareMaxBundle+1))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&bundle); err != nil || dec.Decode(new(any)) != io.EOF {
		return deckShareSummary{}, http.StatusBadGateway, errors.New("invalid deck share bundle from peer")
	}
	if err := s.validateDeckShareBundle(bundle); err != nil {
		return deckShareSummary{}, http.StatusBadGateway, err
	}

	s.shareApplyMu.Lock()
	defer s.shareApplyMu.Unlock()

	snaps, err := s.snapshotDeckShareFiles()
	if err != nil {
		return deckShareSummary{}, http.StatusInternalServerError, err
	}

	summary, oldNAS, err := s.applyDeckShareBundle(peer, bundle)
	if err != nil {
		_ = s.restoreDeckShareFiles(snaps)
		s.reloadAfterDeckShare(oldNAS, nil)
		return deckShareSummary{}, http.StatusInternalServerError, err
	}

	undoPayload, err := json.Marshal(deckShareUndo{Files: snaps, LastCopy: summary})
	if err != nil {
		_ = s.restoreDeckShareFiles(snaps)
		s.reloadAfterDeckShare(oldNAS, nil)
		return deckShareSummary{}, http.StatusInternalServerError, err
	}
	if err := os.MkdirAll(s.copyDir(), 0o700); err != nil {
		_ = s.restoreDeckShareFiles(snaps)
		s.reloadAfterDeckShare(oldNAS, nil)
		return deckShareSummary{}, http.StatusInternalServerError, err
	}
	if err := restoreCopyFile(s.deckShareUndoPath(), copyFileSnapshot{Present: true, Mode: 0o600, Bytes: undoPayload}); err != nil {
		_ = s.restoreDeckShareFiles(snaps)
		s.reloadAfterDeckShare(oldNAS, nil)
		return deckShareSummary{}, http.StatusInternalServerError, err
	}

	s.reloadAfterDeckShare(oldNAS, summary.Copied)
	return summary, http.StatusOK, nil
}

func (s *Server) applyDeckShareBundle(peer string, b deckShareBundle) (deckShareSummary, nasFile, error) {
	sum := deckShareSummary{
		Peer:    peer,
		Board:   b.Board,
		Copied:  []string{},
		Skipped: slices.Clone(b.Skipped),
	}
	s.uiMu.Lock()
	oldNAS, err := s.readNASFileLocked()
	s.uiMu.Unlock()
	if err != nil {
		return sum, oldNAS, err
	}
	if s.cfg.StateDir != "" {
		if err := os.MkdirAll(s.cfg.StateDir, 0o700); err != nil {
			return sum, oldNAS, err
		}
		if b.Deezer != nil {
			if err := providers.SaveJSON(s.deezerOwnerPath(), deezerOwner{UserID: b.Deezer.UserID, ARL: b.Deezer.ARL}); err != nil {
				return sum, oldNAS, err
			}
			sum.Copied = append(sum.Copied, "deezer")
		}
		if b.Netease != nil {
			if err := providers.SaveJSON(filepath.Join(s.cfg.StateDir, "netease-owner.json"), struct {
				Cookie string
				UserID int64
			}{b.Netease.Cookie, b.Netease.UserID}); err != nil {
				return sum, oldNAS, err
			}
			sum.Copied = append(sum.Copied, "netease")
		}
		if b.Spotify != nil {
			if b.Spotify.ClientID != "" {
				if err := restoreCopyFile(s.spotifyClientPath(), copyFileSnapshot{Present: true, Mode: 0o600, Bytes: []byte(b.Spotify.ClientID + "\n")}); err != nil {
					return sum, oldNAS, err
				}
			} else if b.Spotify.Token != nil && b.Spotify.Token.ClientID == providers.SpotifyBuiltinClientID {
				_ = os.Remove(s.spotifyClientPath())
			}
			if b.Spotify.Token != nil {
				if p := s.spotifyTokenFilePath(); p != "" {
					if err := providers.SaveJSON(p, b.Spotify.Token); err != nil {
						return sum, oldNAS, err
					}
				}
			}
			if len(b.Spotify.Links) > 0 {
				if err := restoreCopyFile(filepath.Join(s.cfg.StateDir, "spotify-links.json"), copyFileSnapshot{Present: true, Mode: 0o600, Bytes: b.Spotify.Links}); err != nil {
					return sum, oldNAS, err
				}
			}
			sum.Copied = append(sum.Copied, "spotify")
		}
		if b.AppleMusic != nil {
			_ = os.Remove(filepath.Join(s.cfg.StateDir, "applemusic-owner.json.forgot"))
			if b.AppleMusic.Owner != nil {
				if err := providers.SaveJSON(filepath.Join(s.cfg.StateDir, "applemusic-owner.json"), b.AppleMusic.Owner); err != nil {
					return sum, oldNAS, err
				}
			}
			if b.AppleMusic.Helper != nil {
				cfg := appleMusicHelperSettings{
					AppleMusicHelperConfig: b.AppleMusic.Helper.AppleMusicHelperConfig,
					TokenCleared:           b.AppleMusic.Helper.TokenCleared,
				}
				if err := providers.SaveJSON(s.appleMusicHelperPath(), cfg); err != nil {
					return sum, oldNAS, err
				}
			}
			sum.Copied = append(sum.Copied, "applemusic")
		}
		if b.SoundCloud != nil {
			if err := providers.SaveJSON(s.soundCloudOwnerPath(), soundCloudOwner{
				UserID:   b.SoundCloud.UserID,
				Username: b.SoundCloud.Username,
				ClientID: b.SoundCloud.ClientID,
				Secret:   b.SoundCloud.Secret,
				Token:    b.SoundCloud.Token,
			}); err != nil {
				return sum, oldNAS, err
			}
			_ = os.Remove(filepath.Join(s.cfg.StateDir, "soundcloud-token.json"))
			if s.soundCloudAuth != nil {
				s.soundCloudAuth.Reload()
			}
			if s.cfg.BPM != nil {
				if err := s.cfg.BPM.UpdateConfig(func(keys *providers.BPMConfig) {
					keys.SoundCloudClientID = b.SoundCloud.ClientID
					keys.SoundCloudSecret = b.SoundCloud.Secret
					keys.SoundCloudToken = b.SoundCloud.Token
				}); err != nil {
					return sum, oldNAS, err
				}
			}
			sum.Copied = append(sum.Copied, "soundcloud")
		}
	}

	s.uiMu.Lock()
	defer s.uiMu.Unlock()
	if b.Servers != nil {
		if cur, err := s.readNASFileLocked(); err == nil {
			oldNAS = cur
		}
		f := oldNAS
		mergeList := func(k serverKind, incoming []savedServer, outNames *[]string) {
			dst := f.list(k)
			for _, srv := range incoming {
				root := serverRoot(k, srv.URL)
				if f.Protocol == k.protocol && serverRoot(k, f.URL) == root {
					f.Username, f.Password, f.Enabled = srv.Username, srv.Password, true
					if srv.Name != "" {
						f.Name = srv.Name
					}
				} else if idx := slices.IndexFunc(*dst, func(cur savedServer) bool { return serverRoot(k, cur.URL) == root }); idx >= 0 {
					(*dst)[idx].Name, (*dst)[idx].URL, (*dst)[idx].Username, (*dst)[idx].Password, (*dst)[idx].Enabled = srv.Name, srv.URL, srv.Username, srv.Password, true
				} else if len(serverEntries(k, f)) < maxServers {
					srv.ID = newServerID()
					srv.Enabled = true
					*dst = append(*dst, srv)
				}
				name := strings.TrimSpace(srv.Name)
				if name == "" {
					name = serverHost(srv.URL)
				}
				*outNames = append(*outNames, name)
			}
		}
		if b.Servers.Slot != nil {
			slotCfg := *b.Servers.Slot
			slotCfg.Enabled = true
			name := strings.TrimSpace(b.Servers.Name)
			if name == "" {
				name = serverHost(slotCfg.URL)
			}
			if !slotNamed(f.NASConfig) || nasTarget(f.NASConfig) == nasTarget(slotCfg) {
				f.NASConfig = slotCfg
				f.Name = b.Servers.Name
			} else if k, ok := kindOfProvider(slotCfg.Protocol); ok {
				mergeList(k, []savedServer{{Name: b.Servers.Name, URL: slotCfg.URL, Username: slotCfg.Username, Password: slotCfg.Password, Enabled: true}}, func() *[]string {
					if k == kindWebDAV {
						return &sum.WebDAV
					}
					return &sum.NAS
				}())
				goto mergedLists
			} else {
				f.NASConfig = slotCfg
				f.Name = b.Servers.Name
			}
			if slotCfg.Protocol == webdavScope {
				sum.WebDAV = append(sum.WebDAV, name)
			} else {
				sum.NAS = append(sum.NAS, name)
			}
		}
	mergedLists:
		mergeList(kindWebDAV, b.Servers.WebDAV, &sum.WebDAV)
		mergeList(kindNAS, b.Servers.SMB, &sum.NAS)

		if err := s.saveNASFileLocked(&f); err != nil {
			return sum, oldNAS, err
		}
		s.resetNASIfMoved(oldNAS.NASConfig)
	}
	ui := s.loadUI()
	uiChanged := false
	for _, svc := range sum.Copied {
		if slices.Contains(uiServices, svc) && !slices.Contains(ui.Sources, svc) {
			ui.Sources = append(ui.Sources, svc)
			uiChanged = true
		}
	}
	if len(ui.Sources) > 1 {
		ordered := make([]string, 0, len(ui.Sources))
		for _, svc := range uiServices {
			if slices.Contains(ui.Sources, svc) {
				ordered = append(ordered, svc)
			}
		}
		if !slices.Equal(ordered, ui.Sources) {
			ui.Sources = ordered
			uiChanged = true
		}
	}
	if len(sum.WebDAV) > 0 && ui.WebDAVSwitch != "1" {
		ui.WebDAVSwitch = "1"
		uiChanged = true
	}
	if len(sum.NAS) > 0 && ui.NASSwitch != "1" {
		ui.NASSwitch = "1"
		uiChanged = true
	}
	if uiChanged {
		if err := s.saveUI(ui); err != nil {
			return sum, oldNAS, err
		}
	}
	return sum, oldNAS, nil
}

func (s *Server) undoDeckShare() error {
	s.shareApplyMu.Lock()
	defer s.shareApplyMu.Unlock()
	undo, ok := s.readDeckShareUndo()
	if !ok {
		return errors.New("no copied logins to undo")
	}

	s.uiMu.Lock()
	oldNAS := s.loadNASFileLocked()
	s.uiMu.Unlock()

	if err := s.restoreDeckShareFiles(undo.Files); err != nil {
		return err
	}
	if err := os.Remove(s.deckShareUndoPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	s.reloadAfterDeckShare(oldNAS, nil)
	return nil
}

func (s *Server) reloadAfterDeckShare(oldNAS nasFile, copiedProviders []string) {
	if s.appleMusic != nil {
		_ = s.appleMusic.Reload()
		s.loadAppleMusicHelper()
	}
	if s.soundCloudAuth != nil {
		s.soundCloudAuth.Reload()
	}

	// Reload live owner sessions cleanly.
	sharedSet := map[string]bool{"deezer": true, "netease": true, "spotify": true, "soundcloud": true, "applemusic": true}
	var toClose []interface{ Close() }
	s.mu.Lock()
	for id, sess := range s.sessions {
		if !sess.Guest && sharedSet[sess.Provider] {
			if p, ok := sess.p.(interface{ Close() }); ok {
				toClose = append(toClose, p)
			}
			delete(s.sessions, id)
		}
	}
	for _, prov := range copiedProviders {
		delete(s.ownerDeleted, prov)
	}
	s.mu.Unlock()
	for _, p := range toClose {
		p.Close()
	}

	if s.tidal != nil {
		_ = s.tidal.sources()
		s.tidal.browse.clear()
		s.tidal.searches.clear()
	}

	s.uiMu.Lock()
	nowNAS := s.loadNASFileLocked()
	s.forgetServersLocked(oldNAS, nowNAS)
	s.resetNASIfMoved(oldNAS.NASConfig)
	v := s.loadUI()
	s.applyShadowFlags(v)
	s.setPeerLocalTargets(s.localEnabledTargetHashesLocked(v, nowNAS))
	s.uiMu.Unlock()
	s.peerServers.dirty.Store(true)
	s.notifySharePeers()
}
