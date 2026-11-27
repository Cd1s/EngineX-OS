package webui

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testlibrary"
	"github.com/Cd1s/EngineX-OS/internal/testsmb"
	"github.com/Cd1s/EngineX-OS/pkg/providers"
)

type omniPlaylistProvider struct {
	provider string
	name     string
}

func (p omniPlaylistProvider) Playlists(context.Context) ([]providers.Playlist, error) {
	id := "own"
	if p.provider == "soundcloud" {
		id = "likes"
	} else if p.provider == "spotify" {
		id = "liked"
	}
	return []providers.Playlist{{ID: id, Title: p.name, Count: 1}}, nil
}

func (p omniPlaylistProvider) Tracks(context.Context, string) ([]providers.Track, error) {
	return []providers.Track{{ID: p.provider + "-track", Title: p.name + " track", Provider: p.provider}}, nil
}

type omniFailingDeezer struct{}

func (omniFailingDeezer) Playlists(context.Context) ([]providers.Playlist, error) {
	return nil, fmt.Errorf("deezer: set a public user ID or open a playlist ID")
}

func (omniFailingDeezer) Tracks(context.Context, string) ([]providers.Track, error) {
	return nil, fmt.Errorf("deezer: set a public user ID or open a playlist ID")
}

type omniTrack struct {
	ID    int64  `json:"id"`
	Title string `json:"title"`
}

// omniDrive is a mounted drive whose Engine Library holds one playlist, in an
// order only the nextEntityId chain gives, plus rows OmniSource must not serve.
func omniDrive(t *testing.T) (dbPath, root string, flac []byte) {
	dbPath, root, db := testlibrary.Create(t)
	flac = append([]byte("fLaC"), bytes.Repeat([]byte{7, 1, 9}, 400)...)
	for name, data := range map[string][]byte{"Contents/Artist/One.flac": flac, "Contents/Two.mp3": []byte("ID3 mp3 data"), "Contents/Unsupported.ogg": []byte("OggS")} {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	outside := filepath.Join(t.TempDir(), "secret.flac")
	os.WriteFile(outside, []byte("fLaC secret"), 0o644)
	if err := os.Symlink(outside, filepath.Join(root, "Contents/Link.flac")); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO Track(id,path,filename,title,artist,length,bpmAnalyzed) VALUES
			(1,'../Contents/Artist/One.flac','One.flac','Omni One','Local Artist',300,124.0),
			(2,'../Contents/Two.mp3','Two.mp3','Omni Two','Local Artist',200,0),
			(3,'../../../../etc/passwd','passwd','Omni Escape','',1,0),
			(4,'../Contents/Unsupported.ogg','Unsupported.ogg','Omni Unsupported','',1,0),
			(5,'../Contents/Link.flac','Link.flac','Omni Link','',1,0)`,
		`INSERT INTO Playlist(id,title,parentListId,isPersisted,nextListId,lastEditTime,isExplicitlyExported) VALUES(1,'Club',0,1,0,0,1)`,
		// Inserted backwards: entity 2 (track 1) comes first through nextEntityId.
		`INSERT INTO PlaylistEntity(id,listId,trackId,databaseUuid,nextEntityId,membershipReference) VALUES
			(1,1,2,'test-library-uuid',0,0),(2,1,1,'test-library-uuid',1,0),(3,1,1,'another-library',0,0)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	return dbPath, root, flac
}

func omniServer(t *testing.T, root string, on bool) (*Server, string) {
	return omniServerScope(t, root, on, "deezer")
}

// omniServerScope is a deck with scope selected and OmniSource on or off. nas, when
// given, is the NAS in nas.json's slot when the deck starts.
func omniServerScope(t *testing.T, root string, on bool, scope string, nas ...providers.NASConfig) (*Server, string) {
	ui := t.TempDir()
	conf := "scope=" + scope + "\n"
	if on {
		conf += "omni_source=1\n"
	}
	os.WriteFile(filepath.Join(ui, "settings.conf"), []byte(conf), 0o644)
	for _, cfg := range nas {
		if err := providers.SaveJSON(filepath.Join(ui, "nas.json"), cfg); err != nil {
			t.Fatal(err)
		}
	}
	s := New(Config{AccessToken: "code", UIDir: ui, StateDir: t.TempDir(), DeezerURL: fakeDeezer(t).URL, MediaRoot: filepath.Dir(root)}, nil)
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return s, srv.URL
}

func titles(t *testing.T, items []json.RawMessage) ([]string, map[string]int64) {
	t.Helper()
	var out []string
	ids := map[string]int64{}
	for _, raw := range items {
		var v omniTrack
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatal(err)
		}
		out = append(out, v.Title)
		ids[v.Title] = v.ID
	}
	return out, ids
}

func TestOmniSourceListsDrivePlaylistsInDeezerScope(t *testing.T) {
	_, root, _ := omniDrive(t)
	_, off := omniServer(t, root, false)
	var plain tidalList
	tidalGet(t, off, "/v1/users/1/playlists", 200, &plain)
	if strings.Contains(string(tidalGet(t, off, "/v1/users/1/playlists", 200, nil)), "[USB:") {
		t.Fatal("OmniSource is off by default")
	}

	s, base := omniServer(t, root, true)
	s.mu.Lock()
	s.sessions["sp"] = &Session{ID: "sp", Provider: "spotify", Ready: true, p: fixtureProvider{}, tracks: map[string]providers.Track{}, expires: time.Now().Add(time.Hour)}
	s.mu.Unlock()
	var lists tidalList
	tidalGet(t, base, "/v1/users/1/playlists", 200, &lists)
	if len(lists.Items) != len(plain.Items)+1 {
		t.Fatalf("want the drive's playlist added: %d vs %d", len(lists.Items), len(plain.Items))
	}
	var club tidalPlaylist
	for _, raw := range lists.Items {
		var p tidalPlaylist
		json.Unmarshal(raw, &p)
		switch p.Title {
		case "[USB: " + filepath.Base(root) + "] Club":
			club = p
		}
	}
	if club.NumberOfTracks != 2 {
		t.Fatalf("club %+v", club)
	}
	var tracks tidalList
	tidalGet(t, base, "/v1/playlists/"+club.UUID+"/tracks", 200, &tracks)
	if got, _ := titles(t, tracks.Items); strings.Join(got, "|") != "Omni One|Omni Two" {
		t.Fatalf("Engine's playlist order, own library only: %v", got)
	}
}

func TestOmniSourceDeezerPlaylistsExcludeOtherStreamingServices(t *testing.T) {
	_, root, _ := omniDrive(t)
	s, base := omniServer(t, root, true)
	s.mu.Lock()
	s.sessions["sp"] = &Session{ID: "sp", Provider: "spotify", Ready: true, p: fixtureProvider{}, tracks: map[string]providers.Track{}, expires: time.Now().Add(time.Hour)}
	s.mu.Unlock()

	var lists tidalList
	tidalGet(t, base, "/v1/users/1/playlists", 200, &lists)
	var sawDrive, sawSpotify bool
	for _, raw := range lists.Items {
		var p tidalPlaylist
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatal(err)
		}
		switch {
		case p.Title == "[USB: "+filepath.Base(root)+"] Club":
			sawDrive = true
		case p.Title == "[Spotify] Friend":
			sawSpotify = true
		}
	}
	if !sawDrive {
		t.Fatal("Deezer OmniSource listing lost the drive playlist")
	}
	if sawSpotify {
		t.Fatal("Deezer OmniSource listing leaked a Spotify playlist")
	}
}

func TestOmniSourceOtherScopesKeepOtherServicePlaylists(t *testing.T) {
	_, root, _ := omniDrive(t)
	s, base := omniServerScope(t, root, true, "spotify")
	s.mu.Lock()
	s.sessions["dz"] = &Session{ID: "dz", Provider: "deezer", Ready: true, p: fixtureProvider{}, tracks: map[string]providers.Track{}, expires: time.Now().Add(time.Hour)}
	s.mu.Unlock()

	var lists tidalList
	tidalGet(t, base, "/v1/users/1/playlists", 200, &lists)
	var friendUUID string
	for _, raw := range lists.Items {
		var p tidalPlaylist
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatal(err)
		}
		if p.Title == "[Deezer] Friend" {
			friendUUID = p.UUID
			break
		}
	}
	if friendUUID == "" {
		t.Fatal("non-Deezer OmniSource scope lost the other-service playlist")
	}
	var tracks tidalList
	tidalGet(t, base, "/v1/playlists/"+friendUUID+"/tracks", 200, &tracks)
	if got, _ := titles(t, tracks.Items); len(got) != 1 || got[0] != "Song" {
		t.Fatalf("secondary service tracks: %v", got)
	}
}

func carrierProvider(t *testing.T, scope string) providers.Provider {
	t.Helper()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if scope == "soundcloud" {
			switch r.URL.Path {
			case "/me/playlists", "/me/likes/playlists":
				fmt.Fprint(w, `{"collection":[{"id":101,"title":"Own","track_count":1}]}`)
			case "/me/likes/tracks", "/playlists/101/tracks":
				fmt.Fprint(w, `{"collection":[{"id":201,"title":"Own Track","user":{"username":"Artist"},"duration":8000}]}`)
			default:
				http.NotFound(w, r)
			}
			return
		}
		switch r.URL.Path {
		case "/me/tracks":
			fmt.Fprint(w, `{"items":[{"track":{"id":"liked-track","name":"Own Track","artists":[{"name":"Artist"}],"duration_ms":8000,"album":{"name":"Album"}}}],"next":null,"total":1}`)
		case "/me/playlists":
			fmt.Fprint(w, `{"items":[{"id":"own","name":"Own","tracks":{"total":1}}],"next":null,"total":1}`)
		case "/playlists/own/items":
			fmt.Fprint(w, `{"items":[{"track":{"id":"own-track","name":"Own Track","artists":[{"name":"Artist"}],"duration_ms":8000,"album":{"name":"Album"}}}],"next":null,"total":1}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(backend.Close)
	if scope == "soundcloud" {
		return &providers.SoundCloud{BaseURL: backend.URL, ClientID: "client", Secret: "secret", Token: "token", HTTP: backend.Client()}
	}
	return &providers.Spotify{BaseURL: backend.URL, Token: providers.Token{AccessToken: "token"}, HTTP: backend.Client()}
}

func TestOmniSourceCarrierOpenSequence(t *testing.T) {
	_, root, _ := omniDrive(t)
	nasBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/rest/getPlaylists.view" {
			fmt.Fprint(w, `{"subsonic-response":{"status":"ok","version":"1.16.1","playlists":{"playlist":[{"id":"nas-list","name":"NAS List","songCount":1}]}}}`)
			return
		}
		if r.URL.Path == "/rest/getPlaylist.view" {
			fmt.Fprint(w, `{"subsonic-response":{"status":"ok","version":"1.16.1","playlist":{"id":"nas-list","name":"NAS List","songCount":1,"entry":[{"id":"nas-track","title":"NAS track","artist":"NAS","suffix":"mp3","duration":8}]}}}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer nasBackend.Close()

	for _, scope := range []string{"soundcloud", "spotify"} {
		for _, omni := range []bool{false, true} {
			t.Run(scope+fmt.Sprintf("/omni=%t", omni), func(t *testing.T) {
				s, base := omniServerScope(t, root, omni, scope, providers.NASConfig{Protocol: "subsonic", URL: nasBackend.URL, Enabled: true})
				s.mu.Lock()
				s.sessions["own"] = &Session{ID: "own", Provider: scope, Ready: true, p: carrierProvider(t, scope), expires: time.Now().Add(time.Hour)}
				merged := map[string]string{"soundcloud": "spotify", "spotify": "soundcloud"}[scope]
				s.sessions["merged"] = &Session{ID: "merged", Provider: merged, Ready: true, p: omniPlaylistProvider{provider: merged, name: "Merged"}, expires: time.Now().Add(time.Hour)}
				s.sessions["broken-deezer"] = &Session{ID: "broken-deezer", Provider: "deezer", Ready: true, p: omniFailingDeezer{}, expires: time.Now().Add(time.Hour)}
				s.mu.Unlock()

				if code, _ := uiPost(t, base, "scope="+scope); code != http.StatusOK {
					t.Fatalf("carrier open status=%d", code)
				}
				var first, second tidalList
				tidalGet(t, base, "/v1/users/1/playlists?limit=10&offset=0", 200, &first)
				tidalGet(t, base, "/v1/users/1/playlists?limit=10&offset=10", 200, &second)
				if len(first.Items) == 0 {
					t.Fatalf("carrier first listing empty, second page has %d", len(second.Items))
				}
				if !omni {
					var own tidalPlaylist
					for _, raw := range first.Items {
						json.Unmarshal(raw, &own)
						if own.Title == "Own" {
							var tracks tidalList
							tidalGet(t, base, "/v1/playlists/"+own.UUID+"/tracks", 200, &tracks)
							tidalGet(t, base, "/v1/playlists/"+own.UUID+"/items?limit=10&offset=0", 200, &tracks)
							if len(tracks.Items) != 1 {
								t.Fatalf("off own tracks=%d", len(tracks.Items))
							}
							t.Logf("Own: %d tracks", len(tracks.Items))
							break
						}
					}
					return
				}
				want := map[string]int{"Own": 1, "[" + map[string]string{"soundcloud": "Spotify", "spotify": "SoundCloud"}[scope] + "] Merged": 1, "[USB: " + filepath.Base(root) + "] Club": 2, "[NAS] NAS List": 1}
				for title, count := range want {
					var id string
					for _, raw := range first.Items {
						var p tidalPlaylist
						json.Unmarshal(raw, &p)
						if p.Title == title {
							id = p.UUID
						}
					}
					if id == "" {
						t.Fatalf("carrier listing missing %q", title)
					}
					var tracks tidalList
					tidalGet(t, base, "/v1/playlists/"+id+"/tracks", 200, &tracks)
					tidalGet(t, base, "/v1/playlists/"+id+"/items?limit=10&offset=0", 200, &tracks)
					if len(tracks.Items) != count {
						t.Fatalf("carrier open %q tracks=%d want=%d", title, len(tracks.Items), count)
					}
					t.Logf("%s: %d tracks", title, len(tracks.Items))
				}
			})
		}
	}
}

func TestOmniSourceSearchFindsDriveTracksBesideStreaming(t *testing.T) {
	dbPath, root, _ := omniDrive(t)
	before, _ := os.ReadFile(dbPath)
	s, base := omniServer(t, root, true)
	s.sessions["deezer-search"] = &Session{ID: "deezer-search", Provider: "deezer", Ready: true, p: &providers.Deezer{BaseURL: s.tidal.catalog.BaseURL, UserID: "fixture"}}
	var res struct{ Tracks tidalList }
	tidalGet(t, base, "/v1/search?query=Omni&limit=50&offset=0&types=TRACKS", 200, &res)
	got, _ := titles(t, res.Tracks.Items)
	// Escape (outside the volume) and Unsupported (not an admitted local codec) are left out;
	// Link is listed but cannot play (see the stream test). Deezer's "Found" follows.
	if strings.Join(got, "|") != "Omni One|Omni Two|Omni Link|Found" {
		t.Fatalf("%v", got)
	}
	tidalGet(t, base, "/v1/search?query=%25&limit=50&offset=0", 200, &res)
	if got, _ := titles(t, res.Tracks.Items); strings.Join(got, "|") != "Found" {
		t.Fatalf("LIKE wildcards must be literal: %v", got)
	}
	after, _ := os.ReadFile(dbPath)
	if sha256.Sum256(before) != sha256.Sum256(after) {
		t.Fatal("the drive's m.db changed")
	}
	for _, side := range []string{"-journal", "-wal", "-shm"} {
		if _, err := os.Stat(dbPath + side); err == nil {
			t.Fatal("read-only open left", side)
		}
	}
}

func TestOmniSourcePlaysDriveFilesDirectly(t *testing.T) {
	_, root, flac := omniDrive(t)
	_, base := omniServer(t, root, true)
	var res struct{ Tracks tidalList }
	tidalGet(t, base, "/v1/search?query=Omni&limit=50&offset=0", 200, &res)
	_, ids := titles(t, res.Tracks.Items)
	for title, want := range map[string]string{"Omni One": "flac", "Omni Two": "mp3"} {
		var info struct{ Manifest string }
		tidalGet(t, base, "/v1/tracks/"+strconv.FormatInt(ids[title], 10)+"/playbackinfopostpaywall", 200, &info)
		raw, _ := base64.StdEncoding.DecodeString(info.Manifest)
		var m struct {
			Codecs string
			URLs   []string
		}
		json.Unmarshal(raw, &m)
		if m.Codecs != want || len(m.URLs) != 1 || !strings.HasSuffix(m.URLs[0], "/v1/stream/"+strconv.FormatInt(ids[title], 10)+"."+want) {
			t.Fatalf("%s: %s", title, raw)
		}
	}
	// Range reads straight from the drive, no download job.
	req, _ := http.NewRequest("GET", base+"/v1/stream/"+strconv.FormatInt(ids["Omni One"], 10)+".flac", nil)
	req.Header.Set("Range", "bytes=10-19")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 206 || resp.Header.Get("Content-Type") != "audio/flac" || !bytes.Equal(body, flac[10:20]) {
		t.Fatalf("%d %s %q", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
	// A symlink on the media must not serve a file outside the volume.
	tidalGet(t, base, "/v1/stream/"+strconv.FormatInt(ids["Omni Link"], 10)+".flac", 404, nil)
	var src struct{ Path string }
	tidalGet(t, base, "/v1/enginex/stems/source?id="+strconv.FormatInt(ids["Omni One"], 10), 200, &src)
	if src.Path != filepath.Join(root, "Contents/Artist/One.flac") {
		t.Fatal("Stems use the drive file:", src.Path)
	}
}

func TestOmniSourceToggle(t *testing.T) {
	s, base := omniServer(t, t.TempDir(), false)
	if code, v := uiPost(t, base, "omniSource=1"); code != 200 || !v.OmniSource || !s.omniSource() {
		t.Fatalf("%d %+v", code, v)
	}
	if code, _ := uiPost(t, base, "omniSource=on"); code != 400 {
		t.Fatal(code)
	}
}

func TestOmniSourceDjayIntegration(t *testing.T) {
	mediaRoot := t.TempDir()
	usb1 := filepath.Join(mediaRoot, "USB1")
	audio1 := filepath.Join(usb1, "Music", "DjayTrack1.mp3")
	audio2 := filepath.Join(usb1, "Music", "DjayTrack2.flac")
	if err := os.MkdirAll(filepath.Dir(audio1), 0o755); err != nil {
		t.Fatal(err)
	}
	flacData := append([]byte("fLaC"), bytes.Repeat([]byte{1, 2, 3}, 100)...)
	if err := os.WriteFile(audio1, []byte("ID3 mp3 data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(audio2, flacData, 0o644); err != nil {
		t.Fatal(err)
	}

	dbDir := filepath.Join(usb1, "djay Media Library")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dbDir, "djay_music_library.db")
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.Exec(`
		CREATE TABLE tracks (id INTEGER PRIMARY KEY, title TEXT, artist TEXT, album TEXT, bpm REAL, key TEXT, duration REAL, path TEXT, beatgrid_offset REAL);
		CREATE TABLE cues (track_id INTEGER, slot INTEGER, position_ms REAL, label TEXT, color TEXT);
		CREATE TABLE loops (track_id INTEGER, slot INTEGER, start_ms REAL, length_ms REAL, label TEXT);
		CREATE TABLE playlists (id INTEGER PRIMARY KEY, name TEXT);
		CREATE TABLE playlist_tracks (playlist_id INTEGER, track_id INTEGER, position INTEGER);
	`); err != nil {
		t.Fatal(err)
	}

	if _, err := db.Exec(`
		INSERT INTO tracks (id, title, artist, album, bpm, key, duration, path, beatgrid_offset) VALUES
			(1, 'Djay Beat One', 'Djay Artist', 'Club Album', 128.0, '8A', 210.0, 'Music/DjayTrack1.mp3', 0.15),
			(2, 'Djay Beat Two', 'Other Artist', 'Deep Album', 124.0, '6A', 190.0, 'Music/DjayTrack2.flac', 0.08);
		INSERT INTO cues (track_id, slot, position_ms, label, color) VALUES
			(1, 0, 1000.0, 'Cue 1', '#FF0000');
		INSERT INTO loops (track_id, slot, start_ms, length_ms, label) VALUES
			(1, 0, 30000.0, 8000.0, 'Loop 1');
		INSERT INTO playlists (id, name) VALUES (1, 'House');
		INSERT INTO playlist_tracks (playlist_id, track_id, position) VALUES (1, 1, 1), (1, 2, 2);
	`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	djayDir := filepath.Join(usb1, "djay")
	if err := os.MkdirAll(djayDir, 0o755); err != nil {
		t.Fatal(err)
	}
	m3uContent := "#EXTM3U\n#EXTINF:210,Djay Artist - Djay Beat One\n#EXTDJAY:BPM=128.0,KEY=8A\n../Music/DjayTrack1.mp3\n"
	if err := os.WriteFile(filepath.Join(djayDir, "TopPicks.m3u8"), []byte(m3uContent), 0o644); err != nil {
		t.Fatal(err)
	}

	_, base := omniServer(t, usb1, true)

	// 1. Verify OmniSource sees [djay · USB1] House playlist
	var lists tidalList
	tidalGet(t, base, "/v1/users/1/playlists", 200, &lists)
	var housePl *tidalPlaylist
	for _, raw := range lists.Items {
		var p tidalPlaylist
		json.Unmarshal(raw, &p)
		if p.Title == "[djay · USB1] House" {
			housePl = &p
			break
		}
	}
	if housePl == nil {
		t.Fatalf("expected playlist [djay · USB1] House, got lists: %+v", lists)
	}
	if housePl.NumberOfTracks != 2 {
		t.Fatalf("expected 2 tracks in House playlist, got %d", housePl.NumberOfTracks)
	}

	// 2. Fetch tracks from the playlist
	var trList tidalList
	tidalGet(t, base, "/v1/playlists/"+housePl.UUID+"/tracks", 200, &trList)
	gotTitles, _ := titles(t, trList.Items)
	if strings.Join(gotTitles, "|") != "Djay Beat One|Djay Beat Two" {
		t.Fatalf("expected tracks Djay Beat One|Djay Beat Two, got %v", gotTitles)
	}

	// 3. Search tracks
	var searchRes struct{ Tracks tidalList }
	tidalGet(t, base, "/v1/search?query=Djay&limit=50&offset=0&types=TRACKS", 200, &searchRes)
	foundTitles, ids := titles(t, searchRes.Tracks.Items)
	foundAny := false
	for _, title := range foundTitles {
		if strings.HasPrefix(title, "Djay Beat") {
			foundAny = true
			break
		}
	}
	if !foundAny {
		t.Fatalf("expected search to find Djay tracks, got %v", foundTitles)
	}

	// 4. Test playback streaming of Djay track
	trID := ids["Djay Beat One"]
	if trID == 0 {
		t.Fatalf("expected non-zero track ID for Djay Beat One")
	}
	streamURL := base + "/v1/stream/" + strconv.FormatInt(trID, 10) + ".mp3"
	resp, err := http.Get(streamURL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !bytes.Equal(body, []byte("ID3 mp3 data")) {
		t.Fatalf("unexpected stream response: %d, body=%q", resp.StatusCode, body)
	}
}

// TestOmniSourceReporter59PlaylistsAndUnconfiguredDeezer models the #335 PRIME 4
// setup: 59 NAS playlists (paginated across >50 items), a local USB drive,
// SoundCloud Likes + user playlist, Spotify Liked Songs + user playlist, and an
// unconfigured real Deezer session that logs its user-ID failure on every list
// refresh. Toggling OmniSource live from 0 to 1 keeps every playlist openable.
func TestOmniSourceReporter59PlaylistsAndUnconfiguredDeezer(t *testing.T) {
	_, root, _ := omniDrive(t)

	// 59 Subsonic playlists (always runs, even without smbd).
	subsonicSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/rest/getPlaylists.view":
			pls := make([]map[string]any, 59)
			for i := range pls {
				pls[i] = map[string]any{"id": fmt.Sprintf("nas-%02d", i+1), "name": fmt.Sprintf("Folder %02d", i+1), "songCount": 1}
			}
			json.NewEncoder(w).Encode(map[string]any{
				"subsonic-response": map[string]any{
					"status":    "ok",
					"version":   "1.16.1",
					"playlists": map[string]any{"playlist": pls},
				},
			})
		case "/rest/getPlaylist.view":
			id := r.URL.Query().Get("id")
			json.NewEncoder(w).Encode(map[string]any{
				"subsonic-response": map[string]any{
					"status":  "ok",
					"version": "1.16.1",
					"playlist": map[string]any{
						"id":        id,
						"name":      id,
						"songCount": 1,
						"entry":     []map[string]any{{"id": "tr-" + id, "title": "Track " + id, "artist": "NAS", "suffix": "mp3", "duration": 180}},
					},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer subsonicSrv.Close()

	runCase := func(t *testing.T, nasCfg providers.NASConfig, wantNASFirst, wantNASLast string) {
		for _, scope := range []string{"soundcloud", "spotify"} {
			t.Run(scope, func(t *testing.T) {
				s, base := omniServerScope(t, root, false, scope, nasCfg)
				scProv := carrierProvider(t, "soundcloud")
				spProv := carrierProvider(t, "spotify")
				s.mu.Lock()
				s.sessions["sc-owner"] = &Session{ID: "sc-owner", Provider: "soundcloud", Ready: true, p: scProv, expires: time.Now().Add(time.Hour)}
				s.sessions["sp-owner"] = &Session{ID: "sp-owner", Provider: "spotify", Ready: true, p: spProv, expires: time.Now().Add(time.Hour)}
				// Real unconfigured Deezer session: reproduces "deezer: set a public user ID or open a playlist ID".
				s.sessions["dz-unconfigured"] = &Session{ID: "dz-unconfigured", Provider: "deezer", Ready: true, p: &providers.Deezer{BaseURL: s.tidal.catalog.BaseURL}, expires: time.Now().Add(time.Hour)}
				s.mu.Unlock()

				// 1. OmniSource OFF: open scope, verify own liked/user playlist opens.
				if code, _ := uiPost(t, base, "scope="+scope); code != http.StatusOK {
					t.Fatalf("omni=0 scope=%s status=%d", scope, code)
				}
				var offList tidalList
				tidalGet(t, base, "/v1/users/1/playlists?limit=50&offset=0", 200, &offList)
				if len(offList.Items) < 2 {
					t.Fatalf("omni=0 playlists=%d, want >=2", len(offList.Items))
				}
				var firstOff tidalPlaylist
				json.Unmarshal(offList.Items[0], &firstOff)
				var offTracks tidalList
				tidalGet(t, base, "/v1/playlists/"+firstOff.UUID, 200, nil)
				tidalGet(t, base, "/v1/playlists/"+firstOff.UUID+"/tracks?limit=50&offset=0", 200, &offTracks)
				if len(offTracks.Items) != 1 {
					t.Fatalf("omni=0 open %q tracks=%d", firstOff.Title, len(offTracks.Items))
				}

				// 2. Toggle OmniSource ON live, re-open scope, and paginate across >50 playlists + /favorites/playlists.
				if code, v := uiPost(t, base, "omniSource=1"); code != http.StatusOK || !v.OmniSource {
					t.Fatalf("toggle omniSource=1: code=%d omni=%t", code, v.OmniSource)
				}
				if code, _ := uiPost(t, base, "scope="+scope); code != http.StatusOK {
					t.Fatalf("omni=1 scope=%s status=%d", scope, code)
				}
				var p1, p2, fav tidalList
				tidalGet(t, base, "/v1/users/1/playlists?limit=50&offset=0", 200, &p1)
				tidalGet(t, base, "/v1/users/1/playlists?limit=50&offset=50", 200, &p2)
				tidalGet(t, base, "/v1/users/1/favorites/playlists?limit=50&offset=0", 200, &fav)
				allItems := append(append([]json.RawMessage{}, p1.Items...), p2.Items...)
				if len(allItems) < 62 {
					t.Fatalf("omni=1 total items=%d (p1=%d p2=%d), want >=62", len(allItems), len(p1.Items), len(p2.Items))
				}

				byTitle := map[string]string{}
				for _, raw := range allItems {
					var pl tidalPlaylist
					json.Unmarshal(raw, &pl)
					byTitle[pl.Title] = pl.UUID
				}
				// Verify own liked list, own playlist, merged service liked + playlist, USB, and NAS playlists all open.
				wantTitles := []string{
					firstOff.Title,
					"Own",
					"[USB: " + filepath.Base(root) + "] Club",
					wantNASFirst,
					wantNASLast,
				}
				if scope == "soundcloud" {
					wantTitles = append(wantTitles, "[Spotify] Own")
				} else {
					wantTitles = append(wantTitles, "[SoundCloud] Own")
				}
				for _, title := range wantTitles {
					id := byTitle[title]
					if id == "" {
						t.Fatalf("omni=1 listing missing %q", title)
					}
					tidalGet(t, base, "/v1/playlists/"+id, 200, nil)
					var tr1, tr2 tidalList
					tidalGet(t, base, "/v1/playlists/"+id+"/tracks?limit=50&offset=0", 200, &tr1)
					tidalGet(t, base, "/v1/playlists/"+id+"/items?limit=50&offset=0", 200, &tr2)
					if len(tr1.Items) == 0 || len(tr2.Items) == 0 {
						t.Fatalf("omni=1 playlist %q returned empty tracks/items (%d/%d)", title, len(tr1.Items), len(tr2.Items))
					}
				}
			})
		}
	}

	t.Run("subsonic-59", func(t *testing.T) {
		runCase(t, providers.NASConfig{Protocol: "subsonic", URL: subsonicSrv.URL, Enabled: true}, "[NAS] Folder 01", "[NAS] Folder 59")
	})
	t.Run("smb-59", func(t *testing.T) {
		smbSrv := testsmb.Start(t)
		mp3 := []byte("ID3\x03\x00\x00\x00\x00\x00\x00" + strings.Repeat("\xff\xfb\x90\x00", 64))
		if err := os.WriteFile(filepath.Join(smbSrv.Root, "RootTrack.mp3"), mp3, 0o600); err != nil {
			t.Fatal(err)
		}
		for i := 1; i <= 58; i++ {
			dir := filepath.Join(smbSrv.Root, fmt.Sprintf("Set %02d", i))
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if i == 58 {
				if err := os.WriteFile(filepath.Join(dir, "LateTrack.mp3"), mp3, 0o600); err != nil {
					t.Fatal(err)
				}
			}
		}
		runCase(t, providers.NASConfig{Protocol: "smb", URL: smbSrv.URL, Username: smbSrv.User, Password: smbSrv.Password, Enabled: true}, "[NAS] Root", "[NAS] Set 58")
	})
}
