package providers

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestSelfHostedSubsonic(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		// Verify standard Subsonic params
		if q.Get("v") != "1.16.1" || q.Get("c") != "EngineX" || q.Get("f") != "json" {
			http.Error(w, "bad subsonic params", http.StatusBadRequest)
			return
		}
		if q.Get("u") != "testuser" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		salt := q.Get("s")
		token := q.Get("t")
		expectedHash := md5.Sum([]byte("testpass" + salt))
		if hex.EncodeToString(expectedHash[:]) != token {
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/rest/getPlaylists.view":
			fmt.Fprint(w, `{
				"subsonic-response": {
					"status": "ok",
					"version": "1.16.1",
					"playlists": {
						"playlist": [
							{"id": "pl1", "name": "House Beats", "songCount": 12},
							{"id": "pl2", "name": "Chill Lounge", "songCount": 8}
						]
					}
				}
			}`)
		case "/rest/getPlaylist.view":
			if q.Get("id") != "pl1" {
				http.Error(w, "playlist not found", http.StatusNotFound)
				return
			}
			fmt.Fprint(w, `{
				"subsonic-response": {
					"status": "ok",
					"version": "1.16.1",
					"playlist": {
						"id": "pl1",
						"name": "House Beats",
						"songCount": 1,
						"entry": [
							{
								"id": "s1",
								"title": "Deep Groove",
								"artist": "DJ Sub",
								"album": "Club Session",
								"duration": 240,
								"bpm": 124.0,
								"coverArt": "art1"
							}
						]
					}
				}
			}`)
		case "/rest/search3.view":
			if q.Get("query") != "Deep" {
				http.Error(w, "bad query", http.StatusBadRequest)
				return
			}
			fmt.Fprint(w, `{
				"subsonic-response": {
					"status": "ok",
					"version": "1.16.1",
					"searchResult3": {
						"song": [
							{
								"id": "s1",
								"title": "Deep Groove",
								"artist": "DJ Sub",
								"album": "Club Session",
								"duration": 240,
								"bpm": 124.0,
								"coverArt": "art1"
							}
						]
					}
				}
			}`)
		case "/rest/download.view":
			// Simulate download failing so it falls back to stream.view
			http.Error(w, "download not supported", http.StatusNotImplemented)
		case "/rest/stream.view":
			if q.Get("id") != "s1" || q.Get("format") != "raw" {
				http.Error(w, "bad stream request", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "audio/mpeg")
			w.Header().Set("Content-Disposition", `attachment; filename="deep_groove.mp3"`)
			w.Write([]byte("FAKE-MP3-STREAM-DATA"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	cfg := NASConfig{
		Protocol: "subsonic",
		URL:      ts.URL,
		Username: "testuser",
		Password: "testpass",
		Enabled:  true,
	}
	p := NewSelfHostedProvider(cfg, ts.Client())

	ctx := context.Background()

	// 1. Playlists
	pls, err := p.Playlists(ctx)
	if err != nil {
		t.Fatalf("Playlists failed: %v", err)
	}
	if len(pls) != 2 || pls[0].ID != "pl1" || pls[0].Title != "House Beats" || pls[0].Count != 12 {
		t.Fatalf("unexpected playlists: %+v", pls)
	}

	// 2. PlaylistTracks & Tracks
	tracks, err := p.PlaylistTracks(ctx, "pl1")
	if err != nil {
		t.Fatalf("PlaylistTracks failed: %v", err)
	}
	if len(tracks) != 1 {
		t.Fatalf("expected 1 track, got %d", len(tracks))
	}
	tr := tracks[0]
	if tr.ID != "s1" || tr.Title != "Deep Groove" || tr.Artist != "DJ Sub" || tr.Album != "Club Session" || tr.DurationMS != 240000 || tr.BPM != 124.0 || tr.Provider != "subsonic" {
		t.Fatalf("unexpected track: %+v", tr)
	}
	if !strings.Contains(tr.CoverURL, "/rest/getCoverArt.view?") || !strings.Contains(tr.CoverURL, "id=art1") {
		t.Fatalf("unexpected cover URL: %s", tr.CoverURL)
	}

	// Tracks interface check
	var prov Provider = p
	t2, err := prov.Tracks(ctx, "pl1")
	if err != nil || len(t2) != 1 {
		t.Fatalf("Provider.Tracks failed: %v, len=%d", err, len(t2))
	}

	// 3. Search
	searchResults, err := p.Search(ctx, "Deep")
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}
	if len(searchResults) != 1 || searchResults[0].ID != "s1" {
		t.Fatalf("unexpected search results: %+v", searchResults)
	}

	// 4. DownloadTrack (with fallback to stream.view)
	tmpDir := t.TempDir()
	outPath, err := p.DownloadTrack(ctx, "s1", tmpDir)
	if err != nil {
		t.Fatalf("DownloadTrack failed: %v", err)
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("failed to read downloaded file: %v", err)
	}
	if string(data) != "FAKE-MP3-STREAM-DATA" {
		t.Fatalf("unexpected file contents: %s", string(data))
	}
}

func TestSelfHostedSubsonicTokenAuth(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("p") != "my-api-token" {
			http.Error(w, "bad token", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"subsonic-response": {
				"status": "ok",
				"version": "1.16.1",
				"playlists": {"playlist": []}
			}
		}`)
	}))
	defer ts.Close()

	cfg := NASConfig{
		Protocol: "subsonic",
		URL:      ts.URL,
		Token:    "my-api-token",
		Enabled:  true,
	}
	p := NewSelfHostedProvider(cfg, ts.Client())
	pls, err := p.Playlists(context.Background())
	if err != nil {
		t.Fatalf("Playlists with token failed: %v", err)
	}
	if len(pls) != 0 {
		t.Fatalf("expected 0 playlists, got %d", len(pls))
	}
}

func TestSelfHostedWebDAV(t *testing.T) {
	authHeaderSeen := ""
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeaderSeen = r.Header.Get("Authorization")
		if r.Method == "PROPFIND" {
			w.Header().Set("Content-Type", "application/xml; charset=utf-8")
			w.WriteHeader(207) // Multi-Status
			switch r.URL.Path {
			case "/", "/music", "/music/":
				fmt.Fprint(w, `<?xml version="1.0" encoding="utf-8"?>
<D:multistatus xmlns:D="DAV:">
  <D:response>
    <D:href>/music/</D:href>
    <D:propstat>
      <D:prop>
        <D:resourcetype><D:collection/></D:resourcetype>
      </D:prop>
      <D:status>HTTP/1.1 200 OK</D:status>
    </D:propstat>
  </D:response>
  <D:response>
    <D:href>/music/Electronic/</D:href>
    <D:propstat>
      <D:prop>
        <D:displayname>Electronic Hits</D:displayname>
        <D:resourcetype><D:collection/></D:resourcetype>
      </D:prop>
      <D:status>HTTP/1.1 200 OK</D:status>
    </D:propstat>
  </D:response>
  <D:response>
    <D:href>/music/root_track.flac</D:href>
    <D:propstat>
      <D:prop>
        <D:displayname>Root Anthem</D:displayname>
        <D:getcontentlength>1048576</D:getcontentlength>
        <D:resourcetype/>
      </D:prop>
      <D:status>HTTP/1.1 200 OK</D:status>
    </D:propstat>
  </D:response>
</D:multistatus>`)
			case "/music/Electronic", "/music/Electronic/":
				fmt.Fprint(w, `<?xml version="1.0" encoding="utf-8"?>
<D:multistatus xmlns:D="DAV:">
  <D:response>
    <D:href>/music/Electronic/</D:href>
    <D:propstat>
      <D:prop>
        <D:resourcetype><D:collection/></D:resourcetype>
      </D:prop>
      <D:status>HTTP/1.1 200 OK</D:status>
    </D:propstat>
  </D:response>
  <D:response>
    <D:href>/music/Electronic/synths.mp3</D:href>
    <D:propstat>
      <D:prop>
        <D:displayname>Synths Everywhere</D:displayname>
        <D:getcontentlength>2097152</D:getcontentlength>
        <D:resourcetype/>
      </D:prop>
      <D:status>HTTP/1.1 200 OK</D:status>
    </D:propstat>
  </D:response>
</D:multistatus>`)
			default:
				http.NotFound(w, r)
			}
			return
		}

		if r.Method == http.MethodGet {
			switch r.URL.Path {
			case "/music/root_track.flac":
				w.Write([]byte("FAKE-FLAC-AUDIO"))
			case "/music/Electronic/synths.mp3", "/music/Other/synths.mp3":
				w.Write([]byte("FAKE-MP3-AUDIO"))
			default:
				http.NotFound(w, r)
			}
			return
		}

		http.Error(w, "bad method", http.StatusMethodNotAllowed)
	}))
	defer ts.Close()

	cfg := NASConfig{
		Protocol: "webdav",
		URL:      ts.URL + "/music/",
		Token:    "dav-token-123",
		Enabled:  true,
	}
	p := NewSelfHostedProvider(cfg, ts.Client())
	ctx := context.Background()

	// 1. Playlists: should return root + Electronic Hits
	pls, err := p.Playlists(ctx)
	if err != nil {
		t.Fatalf("WebDAV Playlists failed: %v", err)
	}
	if !strings.HasPrefix(authHeaderSeen, "Bearer dav-token-123") {
		t.Fatalf("expected Bearer token, got %q", authHeaderSeen)
	}
	if len(pls) != 2 {
		t.Fatalf("expected 2 playlists, got %d: %+v", len(pls), pls)
	}
	if pls[0].ID != "root" || pls[0].Count != 1 {
		t.Fatalf("expected root playlist with 1 track, got %+v", pls[0])
	}
	if pls[1].Title != "Electronic Hits" {
		t.Fatalf("expected Electronic Hits, got %+v", pls[1])
	}

	// 2. PlaylistTracks for root
	rootTracks, err := p.PlaylistTracks(ctx, "root")
	if err != nil {
		t.Fatalf("WebDAV PlaylistTracks(root) failed: %v", err)
	}
	if len(rootTracks) != 1 || rootTracks[0].Title != "Root Anthem" || rootTracks[0].Provider != "webdav" {
		t.Fatalf("unexpected root tracks: %+v", rootTracks)
	}

	// 3. PlaylistTracks for subfolder
	subTracks, err := p.PlaylistTracks(ctx, pls[1].ID)
	if err != nil {
		t.Fatalf("WebDAV PlaylistTracks(sub) failed: %v", err)
	}
	if len(subTracks) != 1 || subTracks[0].Title != "Synths Everywhere" || subTracks[0].Provider != "webdav" {
		t.Fatalf("unexpected sub tracks: %+v", subTracks)
	}

	// 4. Search
	sRes, err := p.Search(ctx, "synths")
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}
	if len(sRes) != 1 || sRes[0].Title != "Synths Everywhere" {
		t.Fatalf("unexpected search results: %+v", sRes)
	}

	// 5. DownloadTrack
	tmpDir := t.TempDir()
	dlPath, err := p.DownloadTrack(ctx, "/music/Electronic/synths.mp3", tmpDir)
	if err != nil {
		t.Fatalf("WebDAV DownloadTrack failed: %v", err)
	}
	name := filepath.Base(dlPath)
	digest, hashErr := hex.DecodeString(strings.TrimSuffix(strings.TrimPrefix(name, "nas-"), ".mp3"))
	if filepath.Dir(dlPath) != filepath.Clean(tmpDir) || strings.ContainsAny(name, `/\`) ||
		!strings.HasPrefix(name, "nas-") || filepath.Ext(name) != ".mp3" || hashErr != nil || len(digest) != 32 {
		t.Fatalf("unexpected flat NAS cache filename: %s", dlPath)
	}
	again, err := p.DownloadTrack(ctx, "/music/Electronic/synths.mp3", tmpDir)
	if err != nil || again != dlPath {
		t.Fatalf("unstable cache name: first=%s again=%s error=%v", dlPath, again, err)
	}
	other, err := p.DownloadTrack(ctx, "/music/Other/synths.mp3", tmpDir)
	if err != nil || other == dlPath || filepath.Dir(other) != filepath.Clean(tmpDir) || filepath.Ext(other) != ".mp3" {
		t.Fatalf("same basename in another folder did not get a distinct flat MP3 name: %s, error=%v", other, err)
	}
	data, _ := os.ReadFile(dlPath)
	if string(data) != "FAKE-MP3-AUDIO" {
		t.Fatalf("unexpected downloaded content: %s", string(data))
	}
}

func TestSelfHostedPlex(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("X-Plex-Token")
		if token == "" {
			token = r.URL.Query().Get("X-Plex-Token")
		}
		if token != "plex-secret-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Accept") != "application/json" {
			http.Error(w, "must accept json", http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/playlists":
			if r.URL.Query().Get("playlistType") != "audio" {
				http.Error(w, "bad playlistType", http.StatusBadRequest)
				return
			}
			fmt.Fprint(w, `{
				"MediaContainer": {
					"size": 1,
					"Metadata": [
						{
							"ratingKey": "pl-plex-1",
							"title": "Summer Vibes",
							"leafCount": 5,
							"playlistType": "audio"
						}
					]
				}
			}`)
		case "/playlists/pl-plex-1/items":
			fmt.Fprint(w, `{
				"MediaContainer": {
					"size": 1,
					"Metadata": [
						{
							"ratingKey": "track-plex-101",
							"title": "Sunset Horizon",
							"grandparentTitle": "Sunset Duo",
							"parentTitle": "Golden Hour",
							"duration": 185000,
							"thumb": "/library/metadata/track-plex-101/thumb/123",
							"Media": [
								{
									"id": 201,
									"Part": [
										{
											"id": 301,
											"key": "/library/parts/301/sunset.flac",
											"file": "/media/sunset.flac"
										}
									]
								}
							]
						}
					]
				}
			}`)
		case "/hubs/search":
			query := r.URL.Query().Get("query")
			if query != "Sunset" {
				http.Error(w, "query mismatch", http.StatusBadRequest)
				return
			}
			fmt.Fprint(w, `{
				"MediaContainer": {
					"size": 1,
					"Hub": [
						{
							"type": "track",
							"Metadata": [
								{
									"ratingKey": "track-plex-101",
									"title": "Sunset Horizon",
									"grandparentTitle": "Sunset Duo",
									"parentTitle": "Golden Hour",
									"duration": 185000,
									"Media": [
										{
											"id": 201,
											"Part": [
												{
													"id": 301,
													"key": "/library/parts/301/sunset.flac",
													"file": "/media/sunset.flac"
												}
											]
										}
									]
								}
							]
						}
					]
				}
			}`)
		case "/library/parts/301/sunset.flac":
			w.Header().Set("Content-Type", "audio/flac")
			w.Write([]byte("FAKE-PLEX-FLAC-STREAM"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	cfg := NASConfig{
		Protocol: "plex",
		URL:      ts.URL,
		Token:    "plex-secret-token",
		Enabled:  true,
	}
	p := NewSelfHostedProvider(cfg, ts.Client())
	ctx := context.Background()

	// 1. Playlists
	pls, err := p.Playlists(ctx)
	if err != nil {
		t.Fatalf("Plex Playlists failed: %v", err)
	}
	if len(pls) != 1 || pls[0].ID != "pl-plex-1" || pls[0].Title != "Summer Vibes" || pls[0].Count != 5 {
		t.Fatalf("unexpected playlists: %+v", pls)
	}

	// 2. PlaylistTracks
	tracks, err := p.PlaylistTracks(ctx, "pl-plex-1")
	if err != nil {
		t.Fatalf("Plex PlaylistTracks failed: %v", err)
	}
	if len(tracks) != 1 {
		t.Fatalf("expected 1 track, got %d", len(tracks))
	}
	tr := tracks[0]
	if tr.ID != "/library/parts/301/sunset.flac" || tr.Title != "Sunset Horizon" || tr.Artist != "Sunset Duo" || tr.Album != "Golden Hour" || tr.DurationMS != 185000 || tr.Provider != "plex" {
		t.Fatalf("unexpected Plex track: %+v", tr)
	}

	// 3. Search
	sRes, err := p.Search(ctx, "Sunset")
	if err != nil {
		t.Fatalf("Plex Search failed: %v", err)
	}
	if len(sRes) != 1 || sRes[0].Title != "Sunset Horizon" {
		t.Fatalf("unexpected search results: %+v", sRes)
	}

	// 4. DownloadTrack via Part.key
	tmpDir := t.TempDir()
	dlPath, err := p.DownloadTrack(ctx, tr.ID, tmpDir)
	if err != nil {
		t.Fatalf("Plex DownloadTrack failed: %v", err)
	}
	data, _ := os.ReadFile(dlPath)
	if string(data) != "FAKE-PLEX-FLAC-STREAM" {
		t.Fatalf("unexpected downloaded data: %s", string(data))
	}

	// 5. DownloadTrack via ratingKey (resolves through map)
	dlPath2, err := p.DownloadTrack(ctx, "track-plex-101", tmpDir)
	if err != nil {
		t.Fatalf("Plex DownloadTrack via ratingKey failed: %v", err)
	}
	data2, _ := os.ReadFile(dlPath2)
	if string(data2) != "FAKE-PLEX-FLAC-STREAM" {
		t.Fatalf("unexpected downloaded data: %s", string(data2))
	}
}

func TestSelfHostedSubsonicErrorNotSavedAndWebDAVRelativeBase(t *testing.T) {
	ctx := context.Background()

	// 1. Subsonic HTTP 200 JSON/XML errors must not be saved as .mp3 files.
	subSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rest/download.view":
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			fmt.Fprint(w, `{"subsonic-response":{"status":"failed","error":{"code":70,"message":"Data not found"}}}`)
		case "/rest/stream.view":
			w.Header().Set("Content-Type", "text/xml")
			fmt.Fprint(w, `<subsonic-response status="failed"><error code="70" message="Stream not found"/></subsonic-response>`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer subSrv.Close()

	sub := NewSelfHostedProvider(NASConfig{
		Protocol: "subsonic",
		URL:      subSrv.URL,
		Username: "dj",
		Password: "pw",
		Enabled:  true,
	}, subSrv.Client())
	destDir := t.TempDir()
	if gotPath, err := sub.DownloadTrack(ctx, "999", destDir); err == nil {
		t.Fatalf("Subsonic DownloadTrack saved error payload to %s without error", gotPath)
	}
	if entries, _ := os.ReadDir(destDir); len(entries) != 0 {
		t.Fatalf("Subsonic DownloadTrack left bogus files: %v", entries)
	}

	// 2. WebDAV base URL without trailing slash must preserve base path on relative IDs.
	var requested []string
	davSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested = append(requested, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == "PROPFIND" && r.URL.Path == "/music/sub":
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(207)
			fmt.Fprint(w, `<?xml version="1.0"?><d:multistatus xmlns:d="DAV:"><d:response><d:href>/music/sub/song.flac</d:href><d:propstat><d:prop><d:displayname>song.flac</d:displayname></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`)
		case r.Method == "GET" && r.URL.Path == "/music/sub/song.flac":
			w.Write([]byte("FLAC-DATA"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer davSrv.Close()

	dav := NewSelfHostedProvider(NASConfig{
		Protocol: "webdav",
		URL:      davSrv.URL + "/music", // no trailing slash
		Enabled:  true,
	}, davSrv.Client())
	if _, err := dav.PlaylistTracks(ctx, "sub"); err != nil {
		t.Fatalf("WebDAV PlaylistTracks relative failed: %v (requested=%v)", err, requested)
	}
	if _, err := dav.DownloadTrack(ctx, "sub/song.flac", t.TempDir()); err != nil {
		t.Fatalf("WebDAV DownloadTrack relative failed: %v (requested=%v)", err, requested)
	}
}

func TestSelfHostedErrorsRedactCredentials(t *testing.T) {
	dropSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				_ = conn.Close()
				return
			}
		}
		panic(http.ErrAbortHandler)
	}))
	defer dropSrv.Close()

	const (
		dummyUser  = "dummy-nas-user"
		dummyPass  = "dummy-nas-pass-999"
		dummyToken = "dummy-nas-token-999"
	)
	forbidden := []string{dummyPass, dummyToken, dummyUser, "t=", "s=", "p=", "X-Plex-Token", "?"}
	assertClean := func(t *testing.T, op string, err error, wantSubstr string) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s: expected error from dropped server, got nil", op)
		}
		msg := err.Error()
		for _, bad := range forbidden {
			if strings.Contains(msg, bad) {
				t.Errorf("%s error leaked %q: %s", op, bad, msg)
			}
		}
		if wantSubstr != "" && !strings.Contains(msg, wantSubstr) {
			t.Errorf("%s error missing expected path %q: %s", op, wantSubstr, msg)
		}
	}

	ctx := context.Background()
	t.Run("subsonic-password", func(t *testing.T) {
		p := NewSelfHostedProvider(NASConfig{
			Protocol: "subsonic",
			URL:      dropSrv.URL,
			Username: dummyUser,
			Password: dummyPass,
			Enabled:  true,
		}, dropSrv.Client())
		_, err := p.Playlists(ctx)
		assertClean(t, "Playlists", err, "/rest/getPlaylists.view")
		_, err = p.PlaylistTracks(ctx, "pl-1")
		assertClean(t, "PlaylistTracks", err, "/rest/getPlaylist.view")
		_, err = p.DownloadTrack(ctx, "tr-1", t.TempDir())
		assertClean(t, "DownloadTrack", err, "/rest/stream.view")
	})

	t.Run("subsonic-token", func(t *testing.T) {
		p := NewSelfHostedProvider(NASConfig{
			Protocol: "subsonic",
			URL:      dropSrv.URL,
			Username: dummyUser,
			Token:    dummyToken,
			Enabled:  true,
		}, dropSrv.Client())
		_, err := p.Playlists(ctx)
		assertClean(t, "Playlists", err, "/rest/getPlaylists.view")
		_, err = p.PlaylistTracks(ctx, "pl-1")
		assertClean(t, "PlaylistTracks", err, "/rest/getPlaylist.view")
		_, err = p.DownloadTrack(ctx, "tr-1", t.TempDir())
		assertClean(t, "DownloadTrack", err, "/rest/stream.view")
	})

	t.Run("plex-token", func(t *testing.T) {
		p := NewSelfHostedProvider(NASConfig{
			Protocol: "plex",
			URL:      dropSrv.URL,
			Token:    dummyToken,
			Enabled:  true,
		}, dropSrv.Client())
		_, err := p.Playlists(ctx)
		assertClean(t, "Playlists", err, "/playlists")
		_, err = p.PlaylistTracks(ctx, "pl-1")
		assertClean(t, "PlaylistTracks", err, "/playlists/pl-1/items")
		_, err = p.DownloadTrack(ctx, "/library/parts/1/song.flac", t.TempDir())
		assertClean(t, "DownloadTrack", err, "/library/parts/1/song.flac")
	})

	t.Run("webdav-userinfo-and-query", func(t *testing.T) {
		host := strings.TrimPrefix(dropSrv.URL, "http://")
		p := NewSelfHostedProvider(NASConfig{
			Protocol: "webdav",
			URL:      "http://" + dummyUser + ":" + dummyPass + "@" + host + "/music?X-Plex-Token=" + dummyToken,
			Enabled:  true,
		}, dropSrv.Client())
		_, err := p.Playlists(ctx)
		assertClean(t, "Playlists", err, "/music")
		_, err = p.DownloadTrack(ctx, "sub/song.flac", t.TempDir())
		assertClean(t, "DownloadTrack", err, "/music/sub/song.flac")
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSelfHostedSanitizesRedirectTransportAndUpstreamErrors(t *testing.T) {
	const (
		dummyUser  = "dummy-nas-user"
		dummyPass  = "dummy-nas-pass-999"
		dummyToken = "dummy-nas-token-999"
		redirUser  = "redir-user"
		redirPass  = "redir-pass-999"
		redirToken = "redir-tok-999"
		redirPlex  = "redir-plex-999"
	)
	forbidden := []string{
		dummyUser, dummyPass, dummyToken,
		redirUser, redirPass, redirToken, redirPlex,
		"evil.invalid", "t=", "s=", "p=", "X-Plex-Token", "?",
		"proxy rejected", "forged=1", "\n",
	}
	assertSafe := func(t *testing.T, op string, err error, wantSubstrs ...string) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s: expected error, got nil", op)
		}
		msg := err.Error()
		for _, bad := range forbidden {
			if strings.Contains(msg, bad) {
				t.Errorf("%s error leaked %q: %s", op, bad, msg)
			}
		}
		for _, want := range wantSubstrs {
			if !strings.Contains(msg, want) {
				t.Errorf("%s error missing %q: %s", op, want, msg)
			}
		}
	}

	ctx := context.Background()

	t.Run("malformed-redirect-location", func(t *testing.T) {
		redirSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", "http://"+redirUser+":"+redirPass+"@evil.invalid/steal%zz?u="+redirUser+"&t="+redirToken+"&s=salt&X-Plex-Token="+redirPlex)
			w.WriteHeader(http.StatusFound)
		}))
		defer redirSrv.Close()

		sub := NewSelfHostedProvider(NASConfig{
			Protocol: "subsonic",
			URL:      redirSrv.URL,
			Username: dummyUser,
			Password: dummyPass,
			Enabled:  true,
		}, redirSrv.Client())
		_, err := sub.Playlists(ctx)
		assertSafe(t, "subsonic Playlists", err, "/rest/getPlaylists.view", "redirect failed")
		_, err = sub.PlaylistTracks(ctx, "pl-1")
		assertSafe(t, "subsonic PlaylistTracks", err, "/rest/getPlaylist.view", "redirect failed")
		_, err = sub.DownloadTrack(ctx, "tr-1", t.TempDir())
		assertSafe(t, "subsonic DownloadTrack", err, "/rest/stream.view", "redirect failed")

		plex := NewSelfHostedProvider(NASConfig{
			Protocol: "plex",
			URL:      redirSrv.URL,
			Token:    dummyToken,
			Enabled:  true,
		}, redirSrv.Client())
		_, err = plex.Playlists(ctx)
		assertSafe(t, "plex Playlists", err, "/playlists", "redirect failed")
		_, err = plex.PlaylistTracks(ctx, "pl-1")
		assertSafe(t, "plex PlaylistTracks", err, "/playlists/pl-1/items", "redirect failed")
		_, err = plex.DownloadTrack(ctx, "/library/parts/1/song.flac", t.TempDir())
		assertSafe(t, "plex DownloadTrack", err, "/library/parts/1/song.flac", "redirect failed")

		host := strings.TrimPrefix(redirSrv.URL, "http://")
		dav := NewSelfHostedProvider(NASConfig{
			Protocol: "webdav",
			URL:      "http://" + dummyUser + ":" + dummyPass + "@" + host + "/music?X-Plex-Token=" + dummyToken,
			Enabled:  true,
		}, redirSrv.Client())
		_, err = dav.Playlists(ctx)
		assertSafe(t, "webdav Playlists", err, "/music", "redirect failed")
		_, err = dav.DownloadTrack(ctx, "sub/song.flac", t.TempDir())
		assertSafe(t, "webdav DownloadTrack", err, "/music/sub/song.flac", "redirect failed")
	})

	t.Run("transport-inner-error-url", func(t *testing.T) {
		client := &http.Client{
			Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return nil, fmt.Errorf("proxy rejected %s", r.URL.String())
			}),
		}

		sub := NewSelfHostedProvider(NASConfig{
			Protocol: "subsonic",
			URL:      "http://nas.local:4533",
			Username: dummyUser,
			Token:    dummyToken,
			Enabled:  true,
		}, client)
		_, err := sub.Playlists(ctx)
		assertSafe(t, "subsonic Playlists", err, "/rest/getPlaylists.view", "request failed")
		_, err = sub.DownloadTrack(ctx, "tr-1", t.TempDir())
		assertSafe(t, "subsonic DownloadTrack", err, "/rest/stream.view", "request failed")

		plex := NewSelfHostedProvider(NASConfig{
			Protocol: "plex",
			URL:      "http://nas.local:32400",
			Token:    dummyToken,
			Enabled:  true,
		}, client)
		_, err = plex.Playlists(ctx)
		assertSafe(t, "plex Playlists", err, "/playlists", "request failed")
		_, err = plex.DownloadTrack(ctx, "/library/parts/1/song.flac", t.TempDir())
		assertSafe(t, "plex DownloadTrack", err, "/library/parts/1/song.flac", "request failed")

		dav := NewSelfHostedProvider(NASConfig{
			Protocol: "webdav",
			URL:      "http://" + dummyUser + ":" + dummyPass + "@nas.local:8080/music?X-Plex-Token=" + dummyToken,
			Enabled:  true,
		}, client)
		_, err = dav.Playlists(ctx)
		assertSafe(t, "webdav Playlists", err, "/music", "request failed")
		_, err = dav.DownloadTrack(ctx, "sub/song.flac", t.TempDir())
		assertSafe(t, "webdav DownloadTrack", err, "/music/sub/song.flac", "request failed")
	})

	t.Run("subsonic-upstream-error-message", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/rest/getPlaylists.view", "/rest/download.view":
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"subsonic-response":{"status":"failed","error":{"code":40,"message":"auth failed for u=%s&p=%s\nstream-sync: forged=1"}}}`, dummyUser, dummyPass)
			case "/rest/stream.view":
				w.Header().Set("Content-Type", "text/xml")
				fmt.Fprintf(w, `<subsonic-response status="failed"><error code="70" message="missing track for t=%s&amp;s=salt"/></subsonic-response>`, dummyToken)
			default:
				http.NotFound(w, r)
			}
		}))
		defer srv.Close()

		sub := NewSelfHostedProvider(NASConfig{
			Protocol: "subsonic",
			URL:      srv.URL,
			Username: dummyUser,
			Password: dummyPass,
			Enabled:  true,
		}, srv.Client())
		_, err := sub.Playlists(ctx)
		assertSafe(t, "subsonic JSON upstream error", err, "code 40")
		_, err = sub.DownloadTrack(ctx, "tr-1", t.TempDir())
		assertSafe(t, "subsonic XML upstream error", err, "code 70")
	})

	t.Run("context-canceled-preserved", func(t *testing.T) {
		canceledCtx, cancel := context.WithCancel(context.Background())
		cancel()
		sub := NewSelfHostedProvider(NASConfig{
			Protocol: "subsonic",
			URL:      "http://127.0.0.1:1",
			Username: dummyUser,
			Password: dummyPass,
			Enabled:  true,
		}, http.DefaultClient)
		_, err := sub.Playlists(canceledCtx)
		assertSafe(t, "subsonic canceled", err, "/rest/getPlaylists.view", "request canceled")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected errors.Is(err, context.Canceled), got %v", err)
		}
	})
}

func TestSelfHostedTracksOmitCoverCredentials(t *testing.T) {
	const (
		dummyUser  = "dummy-sub-user"
		dummyPass  = "dummy-sub-pass-123"
		dummyToken = "dummy-plex-tok-456"
	)
	ctx := context.Background()

	t.Run("subsonic", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/rest/getPlaylist.view":
				fmt.Fprint(w, `{"subsonic-response":{"status":"ok","playlist":{"id":"pl1","name":"Set","entry":[{"id":"s1","title":"Song","artist":"DJ","album":"Alb","duration":60,"coverArt":"art-42"}]}}}`)
			case "/rest/search3.view":
				fmt.Fprint(w, `{"subsonic-response":{"status":"ok","searchResult3":{"song":[{"id":"s1","title":"Song","artist":"DJ","album":"Alb","duration":60,"coverArt":"art-42"}]}}}`)
			default:
				http.NotFound(w, r)
			}
		}))
		defer srv.Close()

		p := NewSelfHostedProvider(NASConfig{
			Protocol: "subsonic",
			URL:      srv.URL,
			Username: dummyUser,
			Password: dummyPass,
			Enabled:  true,
		}, srv.Client())
		wantCover := srv.URL + "/rest/getCoverArt.view?id=art-42"

		tracks, err := p.PlaylistTracks(ctx, "pl1")
		if err != nil || len(tracks) != 1 {
			t.Fatalf("PlaylistTracks: tracks=%+v err=%v", tracks, err)
		}
		search, err := p.Search(ctx, "Song")
		if err != nil || len(search) != 1 {
			t.Fatalf("Search: tracks=%+v err=%v", search, err)
		}
		for _, tc := range []struct {
			name string
			url  string
		}{
			{"playlist", tracks[0].CoverURL},
			{"search", search[0].CoverURL},
		} {
			if tc.url != wantCover {
				t.Errorf("%s CoverURL = %q, want %q", tc.name, tc.url, wantCover)
			}
			for _, bad := range []string{dummyUser, dummyPass, "u=", "p=", "t=", "s="} {
				if strings.Contains(tc.url, bad) {
					t.Errorf("%s CoverURL leaked %q: %q", tc.name, bad, tc.url)
				}
			}
		}
	})

	t.Run("plex", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/playlists/pl1/items":
				fmt.Fprint(w, `{"MediaContainer":{"Metadata":[{"ratingKey":"tr1","title":"Song","grandparentTitle":"DJ","parentTitle":"Alb","duration":60000,"thumb":"/library/metadata/tr1/thumb/42","Media":[{"Part":[{"key":"/library/parts/1/song.flac"}]}]}]}}`)
			default:
				http.NotFound(w, r)
			}
		}))
		defer srv.Close()

		p := NewSelfHostedProvider(NASConfig{
			Protocol: "plex",
			URL:      srv.URL,
			Token:    dummyToken,
			Enabled:  true,
		}, srv.Client())
		wantCover := srv.URL + "/library/metadata/tr1/thumb/42"

		tracks, err := p.PlaylistTracks(ctx, "pl1")
		if err != nil || len(tracks) != 1 {
			t.Fatalf("PlaylistTracks: tracks=%+v err=%v", tracks, err)
		}
		if tracks[0].CoverURL != wantCover {
			t.Errorf("plex CoverURL = %q, want %q", tracks[0].CoverURL, wantCover)
		}
		for _, bad := range []string{dummyToken, "X-Plex-Token", "?"} {
			if strings.Contains(tracks[0].CoverURL, bad) {
				t.Errorf("plex CoverURL leaked %q: %q", bad, tracks[0].CoverURL)
			}
		}

		// URL-shaped track ID validation error must not echo raw track ID.
		_, err = p.DownloadTrack(ctx, "http://rawuser:rawsecret@evil.invalid/part?X-Plex-Token="+dummyToken, t.TempDir())
		if err == nil {
			t.Fatal("expected error for URL-shaped plex track ID")
		}
		for _, bad := range []string{"rawuser", "rawsecret", "evil.invalid", "X-Plex-Token", dummyToken} {
			if strings.Contains(err.Error(), bad) {
				t.Errorf("plex DownloadTrack error leaked %q: %v", bad, err)
			}
		}
	})
}

func makeTestJPEG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			img.Set(x, y, color.RGBA{R: 120, G: 80, B: 200, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestSelfHostedFetchCoverAndSanitize(t *testing.T) {
	const (
		dummyUser  = "dummy-cover-user"
		dummyPass  = "dummy-cover-pass-789"
		dummyToken = "dummy-plex-cover-tok-789"
	)
	fakeJPEG := makeTestJPEG(t)
	ctx := context.Background()

	t.Run("sanitize", func(t *testing.T) {
		cases := []struct {
			in, want string
		}{
			{
				in:   "https://user:pass@nas.local:4533/rest/getCoverArt.view?c=EngineX&f=json&id=art-42&p=secret&u=alice&v=1.16.1",
				want: "https://nas.local:4533/rest/getCoverArt.view?id=art-42",
			},
			{
				in:   "http://nas.local:32400/library/metadata/tr1/thumb/42?X-Plex-Token=secret-tok",
				want: "http://nas.local:32400/library/metadata/tr1/thumb/42",
			},
			{
				in:   "https://cdn-images.dzcdn.net/images/cover/abc/500x500.jpg",
				want: "https://cdn-images.dzcdn.net/images/cover/abc/500x500.jpg",
			},
		}
		for _, tc := range cases {
			if got := SanitizeCoverURL(tc.in); got != tc.want {
				t.Errorf("SanitizeCoverURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		}
	})

	t.Run("subsonic-fetch", func(t *testing.T) {
		var hits int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits++
			q := r.URL.Query()
			if r.URL.Path != "/rest/getCoverArt.view" || q.Get("id") != "art-42" || q.Get("u") != dummyUser || (q.Get("p") == "" && (q.Get("t") == "" || q.Get("s") == "")) {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(fakeJPEG)
		}))
		defer srv.Close()

		p := NewSelfHostedProvider(NASConfig{
			Protocol: "subsonic",
			URL:      srv.URL,
			Username: dummyUser,
			Password: dummyPass,
			Enabled:  true,
		}, srv.Client())

		cleanURL := srv.URL + "/rest/getCoverArt.view?id=art-42"
		if !p.MatchesCoverURL(cleanURL) {
			t.Fatalf("MatchesCoverURL(%q) = false", cleanURL)
		}
		got, err := p.FetchCover(ctx, cleanURL, 0)
		if err != nil || !bytes.Equal(got, fakeJPEG) {
			t.Fatalf("FetchCover: len=%d err=%v", len(got), err)
		}
		if hits != 1 {
			t.Fatalf("hits=%d, want 1", hits)
		}
		if p.MatchesCoverURL("https://other.invalid/rest/getCoverArt.view?id=art-42") {
			t.Fatal("MatchesCoverURL accepted unrelated host")
		}
		if _, err := p.FetchCover(ctx, "https://other.invalid/rest/getCoverArt.view?id=art-42", 0); err == nil {
			t.Fatal("FetchCover accepted unrelated host")
		}
	})

	t.Run("plex-fetch-header-and-redirect", func(t *testing.T) {
		var leakedHeader string
		other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			leakedHeader = r.Header.Get("X-Plex-Token")
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(fakeJPEG)
		}))
		defer other.Close()

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("X-Plex-Token") != "" {
				http.Error(w, "token must not be in query", http.StatusBadRequest)
				return
			}
			if r.Header.Get("X-Plex-Token") != dummyToken {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			switch r.URL.Path {
			case "/library/metadata/tr1/thumb/42":
				w.Header().Set("Content-Type", "image/jpeg")
				_, _ = w.Write(fakeJPEG)
			case "/library/metadata/tr1/thumb/redir":
				http.Redirect(w, r, other.URL+"/img.jpg", http.StatusFound)
			default:
				http.NotFound(w, r)
			}
		}))
		defer srv.Close()

		p := NewSelfHostedProvider(NASConfig{
			Protocol: "plex",
			URL:      srv.URL,
			Token:    dummyToken,
			Enabled:  true,
		}, srv.Client())

		got, err := p.FetchCover(ctx, srv.URL+"/library/metadata/tr1/thumb/42", 0)
		if err != nil || !bytes.Equal(got, fakeJPEG) {
			t.Fatalf("FetchCover plex: len=%d err=%v", len(got), err)
		}
		if _, err := p.FetchCover(ctx, srv.URL+"/library/metadata/tr1/thumb/redir", 0); err == nil {
			t.Fatal("FetchCover followed cross-host redirect")
		}
		if leakedHeader != "" {
			t.Fatalf("X-Plex-Token leaked across host redirect: %q", leakedHeader)
		}
	})
}

func TestSelfHostedCrossOriginRedirectsRefusedAndNoReferer(t *testing.T) {
	const (
		dummyUser  = "dummy-redir-user"
		dummyPass  = "dummy-redir-pass-555"
		dummyToken = "dummy-redir-plex-555"
	)
	validJPEG := makeTestJPEG(t)
	ctx := context.Background()

	var otherHits atomic.Int32
	var leakedReferer, leakedPlex, leakedAuth atomic.Value
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		otherHits.Add(1)
		if v := r.Header.Get("Referer"); v != "" {
			leakedReferer.Store(v)
		}
		if v := r.Header.Get("X-Plex-Token"); v != "" {
			leakedPlex.Store(v)
		}
		if v := r.Header.Get("Authorization"); v != "" {
			leakedAuth.Store(v)
		}
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(validJPEG)
	}))
	defer other.Close()

	assertNoCrossOrigin := func(t *testing.T, label string, err error) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s: expected cross-origin redirect error, got nil", label)
		}
		msg := err.Error()
		for _, bad := range []string{dummyUser, dummyPass, dummyToken, "t=", "s=", "p=", "X-Plex-Token", other.URL} {
			if strings.Contains(msg, bad) {
				t.Errorf("%s error leaked %q: %s", label, bad, msg)
			}
		}
		if otherHits.Load() != 0 {
			t.Fatalf("%s reached cross-origin server (%d hits, referer=%v, plex=%v, auth=%v)",
				label, otherHits.Load(), leakedReferer.Load(), leakedPlex.Load(), leakedAuth.Load())
		}
	}

	t.Run("subsonic", func(t *testing.T) {
		var sameOriginReferer string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/rest/getCoverArt.view" && r.URL.Query().Get("id") == "same-redir" {
				http.Redirect(w, r, "/rest/getCoverArt.view?id=same-dest", http.StatusFound)
				return
			}
			if r.URL.Path == "/rest/getCoverArt.view" && r.URL.Query().Get("id") == "same-dest" {
				sameOriginReferer = r.Header.Get("Referer")
				w.Header().Set("Content-Type", "image/jpeg")
				_, _ = w.Write(validJPEG)
				return
			}
			http.Redirect(w, r, other.URL+"/steal", http.StatusFound)
		}))
		defer srv.Close()

		p := NewSelfHostedProvider(NASConfig{
			Protocol: "subsonic",
			URL:      srv.URL,
			Username: dummyUser,
			Password: dummyPass,
			Enabled:  true,
		}, srv.Client())

		_, err := p.FetchCover(ctx, srv.URL+"/rest/getCoverArt.view?id=art-1", 0)
		assertNoCrossOrigin(t, "subsonic FetchCover", err)
		_, err = p.Playlists(ctx)
		assertNoCrossOrigin(t, "subsonic Playlists", err)
		_, err = p.DownloadTrack(ctx, "tr-1", t.TempDir())
		assertNoCrossOrigin(t, "subsonic DownloadTrack", err)

		got, err := p.FetchCover(ctx, srv.URL+"/rest/getCoverArt.view?id=same-redir", 0)
		if err != nil || !bytes.Equal(got, validJPEG) {
			t.Fatalf("subsonic same-origin redirect cover failed: len=%d err=%v", len(got), err)
		}
		if sameOriginReferer != "" {
			t.Fatalf("subsonic same-origin redirect leaked Referer: %q", sameOriginReferer)
		}
	})

	t.Run("plex", func(t *testing.T) {
		var queryTokenSeen atomic.Bool
		var sameOriginReferer string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("X-Plex-Token") != "" {
				queryTokenSeen.Store(true)
			}
			if r.URL.Path == "/library/metadata/1/thumb/same" {
				http.Redirect(w, r, "/library/metadata/1/thumb/final", http.StatusFound)
				return
			}
			if r.URL.Path == "/library/metadata/1/thumb/final" {
				sameOriginReferer = r.Header.Get("Referer")
				w.Header().Set("Content-Type", "image/jpeg")
				_, _ = w.Write(validJPEG)
				return
			}
			http.Redirect(w, r, other.URL+"/steal", http.StatusFound)
		}))
		defer srv.Close()

		p := NewSelfHostedProvider(NASConfig{
			Protocol: "plex",
			URL:      srv.URL,
			Token:    dummyToken,
			Enabled:  true,
		}, srv.Client())

		_, err := p.Playlists(ctx)
		assertNoCrossOrigin(t, "plex Playlists", err)
		_, err = p.DownloadTrack(ctx, "/library/parts/1/song.flac", t.TempDir())
		assertNoCrossOrigin(t, "plex DownloadTrack", err)
		_, err = p.FetchCover(ctx, srv.URL+"/library/metadata/1/thumb/redir", 0)
		assertNoCrossOrigin(t, "plex FetchCover", err)
		if queryTokenSeen.Load() {
			t.Fatal("plex sent X-Plex-Token in URL query instead of header-only")
		}

		got, err := p.FetchCover(ctx, srv.URL+"/library/metadata/1/thumb/same", 0)
		if err != nil || !bytes.Equal(got, validJPEG) {
			t.Fatalf("plex same-origin redirect cover failed: len=%d err=%v", len(got), err)
		}
		if sameOriginReferer != "" {
			t.Fatalf("plex same-origin redirect leaked Referer: %q", sameOriginReferer)
		}
	})

	t.Run("webdav", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, other.URL+"/steal", http.StatusFound)
		}))
		defer srv.Close()

		p := NewSelfHostedProvider(NASConfig{
			Protocol: "webdav",
			URL:      srv.URL + "/music/",
			Username: dummyUser,
			Password: dummyPass,
			Enabled:  true,
		}, srv.Client())

		_, err := p.Playlists(ctx)
		assertNoCrossOrigin(t, "webdav Playlists", err)
		_, err = p.DownloadTrack(ctx, "/music/song.flac", t.TempDir())
		assertNoCrossOrigin(t, "webdav DownloadTrack", err)
	})
}

func TestSelfHostedCoverRouteAndImageValidation(t *testing.T) {
	validJPEG := makeTestJPEG(t)
	ctx := context.Background()

	var nonCoverHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/library/metadata/1/thumb/42":
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(validJPEG)
		case "/library/metadata/1/thumb/html":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html><body>private</body></html>"))
		case "/library/metadata/1/thumb/fakejpg":
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write([]byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00, 0xff, 0xd9})
		default:
			nonCoverHits.Add(1)
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte("<MediaContainer secret='1'/>"))
		}
	}))
	defer srv.Close()

	p := NewSelfHostedProvider(NASConfig{
		Protocol: "plex",
		URL:      srv.URL,
		Token:    "plex-secret-tok",
		Enabled:  true,
	}, srv.Client())

	for _, bad := range []string{
		srv.URL + "/:/prefs",
		srv.URL + "/library/parts/1/song.flac",
		srv.URL + "/library/metadata/1",
		srv.URL + "/library/metadata/1/thumb/../../prefs",
		srv.URL + "/library/metadata/1/thumb/%2e%2e/prefs",
		srv.URL + "/library/metadata/1/thumb/42?X-Plex-Token=x",
	} {
		if p.MatchesCoverURL(bad) {
			t.Errorf("MatchesCoverURL accepted non-cover Plex URL %q", bad)
		}
		if _, err := p.FetchCover(ctx, bad, 0); err == nil {
			t.Errorf("FetchCover accepted non-cover Plex URL %q", bad)
		}
	}
	if nonCoverHits.Load() != 0 {
		t.Fatalf("non-cover Plex routes were contacted %d times", nonCoverHits.Load())
	}

	if _, err := p.FetchCover(ctx, srv.URL+"/library/metadata/1/thumb/html", 0); err == nil {
		t.Fatal("FetchCover accepted HTML response body")
	}
	if _, err := p.FetchCover(ctx, srv.URL+"/library/metadata/1/thumb/fakejpg", 0); err == nil {
		t.Fatal("FetchCover accepted header-only truncated JPEG")
	}
	got, err := p.FetchCover(ctx, srv.URL+"/library/metadata/1/thumb/42", 0)
	if err != nil || !bytes.Equal(got, validJPEG) {
		t.Fatalf("FetchCover valid JPEG failed: len=%d err=%v", len(got), err)
	}
}

func TestSelfHostedURLShapedIDsStripCredentialsAndResolvePrivateLocator(t *testing.T) {
	ctx := context.Background()

	t.Run("plex-signed-part-key", func(t *testing.T) {
		const (
			plexUser   = "plex-u-secret"
			plexPass   = "plex-p-secret"
			plexTok    = "plex-tok-secret"
			signedPart = "sig-part-secret"
		)
		var srvURL string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tok := r.Header.Get("X-Plex-Token")
			if tok == "" {
				tok = r.URL.Query().Get("X-Plex-Token")
			}
			if tok != plexTok {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			host := strings.TrimPrefix(srvURL, "http://")
			switch r.URL.Path {
			case "/playlists":
				fmt.Fprintf(w, `{"MediaContainer":{"Metadata":[{"key":"http://%s:%s@%s/playlists/5/items?X-Plex-Token=%s","title":"Signed List","leafCount":2}]}}`,
					plexUser, plexPass, host, plexTok)
			case "/playlists/pl-1/items":
				fmt.Fprintf(w, `{"MediaContainer":{"Metadata":[
					{"ratingKey":"rk-301","title":"With RatingKey","Media":[{"Part":[{"key":"http://%s:%s@%s/library/parts/301/song.flac?X-Plex-Token=%s&sig=%s"}]}]},
					{"title":"No RatingKey","Media":[{"Part":[{"key":"/library/parts/302/other.flac?X-Plex-Token=%s&sig=%s"}]}]}
				]}}`, plexUser, plexPass, host, plexTok, signedPart, plexTok, signedPart)
			case "/library/metadata/rk-301":
				fmt.Fprintf(w, `{"MediaContainer":{"Metadata":[{"ratingKey":"rk-301","title":"With RatingKey","Media":[{"Part":[{"key":"http://%s:%s@%s/library/parts/301/song.flac?X-Plex-Token=%s&sig=%s"}]}]}]}}`,
					plexUser, plexPass, host, plexTok, signedPart)
			case "/library/parts/301/song.flac":
				if r.URL.Query().Get("sig") != signedPart {
					http.Error(w, "missing private sig", http.StatusForbidden)
					return
				}
				_, _ = w.Write([]byte("SIGNED-PLEX-FLAC-1"))
			case "/library/parts/302/other.flac":
				if r.URL.Query().Get("sig") != signedPart {
					http.Error(w, "missing private sig", http.StatusForbidden)
					return
				}
				_, _ = w.Write([]byte("SIGNED-PLEX-FLAC-2"))
			default:
				http.NotFound(w, r)
			}
		}))
		defer srv.Close()
		srvURL = srv.URL

		cfg := NASConfig{Protocol: "plex", URL: srv.URL, Token: plexTok, Enabled: true}
		p := NewSelfHostedProvider(cfg, srv.Client())

		pls, err := p.Playlists(ctx)
		if err != nil || len(pls) != 1 {
			t.Fatalf("Playlists: len=%d err=%v", len(pls), err)
		}
		if pls[0].ID != "/playlists/5/items" {
			t.Fatalf("Playlist ID leaked credentials or was not normalized: %q", pls[0].ID)
		}

		tracks, err := p.PlaylistTracks(ctx, "pl-1")
		if err != nil || len(tracks) != 2 {
			t.Fatalf("PlaylistTracks: len=%d err=%v", len(tracks), err)
		}
		for _, tr := range tracks {
			for _, field := range []string{tr.ID, tr.Preview, tr.CoverURL} {
				for _, bad := range []string{plexUser, plexPass, plexTok, signedPart, "X-Plex-Token", "?", "://"} {
					if strings.Contains(field, bad) {
						t.Errorf("Plex track field leaked %q: %+v", bad, tr)
					}
				}
			}
		}
		if tracks[0].ID != "rk-301" || tracks[1].ID != "/library/parts/302/other.flac" {
			t.Fatalf("unexpected clean Plex track IDs: %q, %q", tracks[0].ID, tracks[1].ID)
		}

		// Same provider instance resolves both private locators.
		for i, want := range []string{"SIGNED-PLEX-FLAC-1", "SIGNED-PLEX-FLAC-2"} {
			fp, err := p.DownloadTrack(ctx, tracks[i].ID, t.TempDir())
			if err != nil {
				t.Fatalf("DownloadTrack[%d] (%q): %v", i, tracks[i].ID, err)
			}
			b, _ := os.ReadFile(fp)
			if string(b) != want {
				t.Fatalf("DownloadTrack[%d] body=%q want %q", i, string(b), want)
			}
		}

		// Fresh provider instance (like webui ownerSelfHosted) still resolves ratingKey via metadata.
		fresh := NewSelfHostedProvider(cfg, srv.Client())
		fp, err := fresh.DownloadTrack(ctx, tracks[0].ID, t.TempDir())
		if err != nil {
			t.Fatalf("fresh DownloadTrack(%q): %v", tracks[0].ID, err)
		}
		b, _ := os.ReadFile(fp)
		if string(b) != "SIGNED-PLEX-FLAC-1" {
			t.Fatalf("fresh DownloadTrack body=%q", string(b))
		}
	})

	t.Run("webdav-signed-href", func(t *testing.T) {
		const (
			davUser = "dav-u-secret"
			davPass = "dav-p-secret"
			dirTok  = "dav-dir-secret"
			fileSig = "dav-file-secret"
		)
		var srvURL string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			host := strings.TrimPrefix(srvURL, "http://")
			switch {
			case r.Method == "PROPFIND" && r.URL.Path == "/music/":
				fmt.Fprintf(w, `<?xml version="1.0"?><D:multistatus xmlns:D="DAV:">
					<D:response><D:href>/music/</D:href><D:propstat><D:prop><D:resourcetype><D:collection/></D:resourcetype></D:prop></D:propstat></D:response>
					<D:response><D:href>http://%s:%s@%s/music/Mixes/?token=%s</D:href><D:propstat><D:prop><D:displayname>Mixes</D:displayname><D:resourcetype><D:collection/></D:resourcetype></D:prop></D:propstat></D:response>
				</D:multistatus>`, davUser, davPass, host, dirTok)
			case r.Method == "PROPFIND" && r.URL.Path == "/music/Mixes/":
				if r.URL.Query().Get("token") != dirTok {
					http.Error(w, "missing dir token", http.StatusForbidden)
					return
				}
				fmt.Fprintf(w, `<?xml version="1.0"?><D:multistatus xmlns:D="DAV:">
					<D:response><D:href>/music/Mixes/</D:href><D:propstat><D:prop><D:resourcetype><D:collection/></D:resourcetype></D:prop></D:propstat></D:response>
					<D:response><D:href>http://%s:%s@%s/music/Mixes/track.flac?sig=%s</D:href><D:propstat><D:prop><D:displayname>track.flac</D:displayname></D:prop></D:propstat></D:response>
				</D:multistatus>`, davUser, davPass, host, fileSig)
			case r.Method == "GET" && r.URL.Path == "/music/Mixes/track.flac":
				if r.URL.Query().Get("sig") != fileSig {
					http.Error(w, "missing file sig", http.StatusForbidden)
					return
				}
				_, _ = w.Write([]byte("SIGNED-DAV-FLAC"))
			default:
				http.NotFound(w, r)
			}
		}))
		defer srv.Close()
		srvURL = srv.URL

		p := NewSelfHostedProvider(NASConfig{
			Protocol: "webdav",
			URL:      srv.URL + "/music/",
			Token:    "dav-bearer",
			Enabled:  true,
		}, srv.Client())

		pls, err := p.Playlists(ctx)
		if err != nil || len(pls) != 2 {
			t.Fatalf("WebDAV Playlists: len=%d err=%v", len(pls), err)
		}
		if pls[1].ID != "/music/Mixes/" {
			t.Fatalf("WebDAV Playlist ID not clean: %q", pls[1].ID)
		}
		tracks, err := p.PlaylistTracks(ctx, pls[1].ID)
		if err != nil || len(tracks) != 1 {
			t.Fatalf("WebDAV PlaylistTracks: len=%d err=%v", len(tracks), err)
		}
		if tracks[0].ID != "/music/Mixes/track.flac" || tracks[0].Album != "/music/Mixes/" {
			t.Fatalf("WebDAV Track fields not clean: %+v", tracks[0])
		}
		for _, field := range []string{pls[1].ID, tracks[0].ID, tracks[0].Album} {
			for _, bad := range []string{davUser, davPass, dirTok, fileSig, "?", "://"} {
				if strings.Contains(field, bad) {
					t.Errorf("WebDAV field leaked %q: %q", bad, field)
				}
			}
		}
		fp, err := p.DownloadTrack(ctx, tracks[0].ID, t.TempDir())
		if err != nil {
			t.Fatalf("WebDAV DownloadTrack: %v", err)
		}
		b, _ := os.ReadFile(fp)
		if string(b) != "SIGNED-DAV-FLAC" {
			t.Fatalf("WebDAV body=%q", string(b))
		}
	})
}
