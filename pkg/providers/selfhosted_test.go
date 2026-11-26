package providers

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
