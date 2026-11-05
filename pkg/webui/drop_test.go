package webui

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testaudio"
	"github.com/Cd1s/EngineX-OS/internal/testlibrary"
	"github.com/Cd1s/EngineX-OS/pkg/analyzer"
	"github.com/Cd1s/EngineX-OS/pkg/deemixclient"
	"github.com/Cd1s/EngineX-OS/pkg/resourceguard"
)

func dropPost(t *testing.T, srv *httptest.Server, query string, files map[string][]byte, token string) (int, dropResult) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for name, data := range files {
		fw, _ := mw.CreateFormFile("file", name)
		fw.Write(data)
	}
	mw.Close()
	req, _ := http.NewRequest("POST", srv.URL+"/api/drop?"+query, &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var res dropResult
	json.NewDecoder(resp.Body).Decode(&res)
	return resp.StatusCode, res
}

func TestWiFiDropSavesSafely(t *testing.T) {
	media := t.TempDir()
	usb := filepath.Join(media, "USB 1")
	os.MkdirAll(usb, 0o755)
	os.MkdirAll(filepath.Join(media, ".hidden"), 0o755)
	outside := t.TempDir()
	os.Symlink(outside, filepath.Join(media, "linked"))
	ui := New(Config{AccessToken: "c", MediaRoot: media, StateDir: t.TempDir()}, nil)
	srv := httptest.NewServer(ui)
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/api/drives", nil)
	req.Header.Set("Authorization", "Bearer c")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var drives []dropDrive
	json.NewDecoder(resp.Body).Decode(&drives)
	resp.Body.Close()
	if len(drives) != 1 || drives[0].Name != "USB 1" || drives[0].Free <= 0 || drives[0].Total < drives[0].Free {
		t.Fatalf("drives: %+v", drives)
	}

	if code, _ := dropPost(t, srv, "drive=USB+1", map[string][]byte{"a.mp3": []byte("x")}, ""); code != 401 {
		t.Fatalf("no login: %d", code)
	}
	code, res := dropPost(t, srv, "drive=USB+1&playlist=Set+1", map[string][]byte{"../../evil.mp3": []byte("one"), `C:\Music\Artist: Track.flac`: []byte("two")}, "c")
	if code != 200 || len(res.Saved) != 2 {
		t.Fatalf("%d %+v", code, res)
	}
	set := filepath.Join(usb, "Wi-Fi Drop", "Set 1")
	if b, _ := os.ReadFile(filepath.Join(set, "evil.mp3")); string(b) != "one" {
		t.Fatal("traversal name not reduced to its base")
	}
	if b, _ := os.ReadFile(filepath.Join(set, "Artist_ Track.flac")); string(b) != "two" {
		t.Fatal("Windows path / FAT-invalid name not cleaned")
	}
	// The same name again is kept next to the first, never over it.
	if code, res = dropPost(t, srv, "drive=USB+1&playlist=Set+1", map[string][]byte{"evil.mp3": []byte("three")}, "c"); code != 200 || res.Saved[0] != "Wi-Fi Drop/Set 1/evil (2).mp3" {
		t.Fatalf("%d %+v", code, res)
	}
	if b, _ := os.ReadFile(filepath.Join(set, "evil.mp3")); string(b) != "one" {
		t.Fatal("existing file overwritten")
	}
	// Default folder, then refusals.
	if code, res = dropPost(t, srv, "drive=USB+1", map[string][]byte{"b.wav": []byte("w")}, "c"); code != 200 || res.Saved[0] != "Wi-Fi Drop/Wi-Fi Drop/b.wav" {
		t.Fatalf("default playlist: %d %+v", code, res)
	}
	for q, files := range map[string]map[string][]byte{
		"drive=linked":                 {"a.mp3": []byte("x")},
		"drive=.hidden":                {"a.mp3": []byte("x")},
		"drive=nope":                   {"a.mp3": []byte("x")},
		"drive=USB+1&playlist=..":      {"a.mp3": []byte("x")},
		"drive=USB+1&playlist=a%2Fb":   {"a.mp3": []byte("x")},
		"drive=USB+1&playlist=Set+1":   {"run.exe": []byte("MZ")},
		"drive=USB+1&playlist=Empty+1": {"e.mp3": nil},
	} {
		if code, _ := dropPost(t, srv, q, files, "c"); code < 400 {
			t.Errorf("%s accepted: %d", q, code)
		}
	}
	if _, err := os.Stat(filepath.Join(usb, "Wi-Fi Drop", "Empty 1", "e.mp3")); err == nil {
		t.Error("empty file kept")
	}
	// A symlinked Wi-Fi Drop folder is refused, so nothing lands outside the drive.
	os.RemoveAll(filepath.Join(usb, "Wi-Fi Drop"))
	os.Symlink(outside, filepath.Join(usb, "Wi-Fi Drop"))
	if code, _ := dropPost(t, srv, "drive=USB+1", map[string][]byte{"a.mp3": []byte("x")}, "c"); code != 500 {
		t.Fatalf("symlinked folder: %d", code)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("wrote outside the drive: %v", entries)
	}
	filepath.Walk(media, func(p string, _ os.FileInfo, _ error) error {
		if strings.HasSuffix(p, ".part") {
			t.Errorf("leftover %s", p)
		}
		return nil
	})
}

func TestDriveKind(t *testing.T) {
	for _, c := range []struct{ dev, sys, want string }{
		{"/dev/mmcblk0p7", "/sys/devices/platform/mmc/mmcblk0/mmcblk0p7", "internal"}, // az01-internal eMMC
		{"/dev/mmcblk1p1", "/sys/devices/platform/mmc/mmcblk1/mmcblk1p1", "sd"},
		{"/dev/mmcblk2p1", "/sys/devices/platform/mmc/mmcblk2/mmcblk2p1", "sd"},
		{"/dev/sda1", "/sys/devices/platform/usb/usb1/1-1/host0/block/sda/sda1", "usb"},
		{"/dev/sdb1", "/sys/devices/platform/ahci/ata1/host1/block/sdb/sdb1", "hd"},
		{"", "", "drive"},
	} {
		if got := driveKind(c.dev, c.sys); got != c.want {
			t.Errorf("%s: %s, want %s", c.dev, got, c.want)
		}
	}
}

// A FLAC dropped on the drive stream-sync imports into is analysed and lands
// in a playlist named after the drop folder.
func TestWiFiDropImportsIntoLibraryDrive(t *testing.T) {
	path, root, db := testlibrary.Create(t)
	lib, err := analyzer.OpenLibrary(path)
	if err != nil {
		t.Fatal(err)
	}
	defer lib.Close()
	var flac bytes.Buffer
	if err := testaudio.FLAC(&flac, testaudio.Song(8)); err != nil {
		t.Fatal(err)
	}
	jobs := NewJobs(Pipeline{Guard: resourceguard.New(resourceguard.Config{Disabled: true}), Catalog: fixtureCatalog{}, Deemix: &deemixclient.Client{DownloadDir: t.TempDir()}, Library: lib})
	defer jobs.Close()
	ui := New(Config{AccessToken: "c", MediaRoot: filepath.Dir(root), StateDir: t.TempDir()}, jobs)
	srv := httptest.NewServer(ui)
	defer srv.Close()
	code, res := dropPost(t, srv, "drive="+filepath.Base(root)+"&playlist=Friday", map[string][]byte{"set opener.flac": flac.Bytes(), "notes.wav": testaudio.WAV(testaudio.Song(1))}, "c")
	if code != 200 || len(res.Saved) != 2 || res.Imported != 1 {
		t.Fatalf("%d %+v", code, res)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		var title string
		err := db.QueryRow("SELECT p.title FROM Track t JOIN PlaylistEntity pe ON pe.trackId=t.id JOIN Playlist p ON p.id=pe.listId WHERE t.filename='set opener.flac'").Scan(&title)
		if err == nil && title == "Friday" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not imported: %v %q %+v", err, title, jobs.List())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// stream-sync runs with ReadTimeout 30 s; an upload slower than the server's
// read timeout must still finish.
func TestWiFiDropOutlivesServerReadTimeout(t *testing.T) {
	media := t.TempDir()
	os.MkdirAll(filepath.Join(media, "SD"), 0o755)
	srv := httptest.NewUnstartedServer(New(Config{AccessToken: "c", MediaRoot: media, StateDir: t.TempDir()}, nil))
	srv.Config.ReadTimeout = 300 * time.Millisecond
	srv.Start()
	defer srv.Close()
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		fw, _ := mw.CreateFormFile("file", "slow.flac")
		for i := 0; i < 8; i++ {
			fw.Write(bytes.Repeat([]byte{byte(i)}, 4096))
			time.Sleep(100 * time.Millisecond)
		}
		mw.Close()
		pw.Close()
	}()
	req, _ := http.NewRequest("POST", srv.URL+"/api/drop?drive=SD", pr)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer c")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	st, err := os.Stat(filepath.Join(media, "SD", "Wi-Fi Drop", "Wi-Fi Drop", "slow.flac"))
	if resp.StatusCode != 200 || err != nil || st.Size() != 8*4096 {
		t.Fatalf("%d %v", resp.StatusCode, err)
	}
}
