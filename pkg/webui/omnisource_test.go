package webui

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
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
	"github.com/Cd1s/EngineX-OS/pkg/providers"
)

type omniTrack struct {
	ID    int64  `json:"id"`
	Title string `json:"title"`
}

// omniDrive is a mounted drive whose Engine Library holds one playlist, in an
// order only the nextEntityId chain gives, plus rows OmniSource must not serve.
func omniDrive(t *testing.T) (dbPath, root string, flac []byte) {
	dbPath, root, db := testlibrary.Create(t)
	flac = append([]byte("fLaC"), bytes.Repeat([]byte{7, 1, 9}, 400)...)
	for name, data := range map[string][]byte{"Contents/Artist/One.flac": flac, "Contents/Two.mp3": []byte("ID3 mp3 data"), "Contents/Wav.wav": []byte("RIFF")} {
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
			(4,'../Contents/Wav.wav','Wav.wav','Omni Wav','',1,0),
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
	ui := t.TempDir()
	conf := "scope=deezer\n"
	if on {
		conf += "omni_source=1\n"
	}
	os.WriteFile(filepath.Join(ui, "settings.conf"), []byte(conf), 0o644)
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

func TestOmniSourceListsDrivePlaylistsAndOtherServices(t *testing.T) {
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
	if len(lists.Items) != len(plain.Items)+2 {
		t.Fatalf("want the drive's and Spotify's playlists added: %d vs %d", len(lists.Items), len(plain.Items))
	}
	var club, friend tidalPlaylist
	for _, raw := range lists.Items {
		var p tidalPlaylist
		json.Unmarshal(raw, &p)
		switch p.Title {
		case "[USB: " + filepath.Base(root) + "] Club":
			club = p
		case "[Spotify] Friend":
			friend = p
		}
	}
	if club.NumberOfTracks != 2 || friend.UUID == "" {
		t.Fatalf("club %+v friend %+v", club, friend)
	}
	var tracks tidalList
	tidalGet(t, base, "/v1/playlists/"+club.UUID+"/tracks", 200, &tracks)
	if got, _ := titles(t, tracks.Items); strings.Join(got, "|") != "Omni One|Omni Two" {
		t.Fatalf("Engine's playlist order, own library only: %v", got)
	}
	tidalGet(t, base, "/v1/playlists/"+friend.UUID+"/tracks", 200, &tracks)
	if got, _ := titles(t, tracks.Items); len(got) != 1 || got[0] != "Song" {
		t.Fatalf("secondary service tracks: %v", got)
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
	// Escape (outside the volume) and Wav (not a stream codec) are left out;
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
