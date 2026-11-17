package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testsmb"
	"github.com/Cd1s/EngineX-OS/pkg/prolink"
)

func TestSMBRekordboxRangeAndSharedCache(t *testing.T) {
	smb := testsmb.Start(t)
	data := append([]byte("fLaC"), bytes.Repeat([]byte("audio"), 2<<20)...)
	os.WriteFile(filepath.Join(smb.Root, "Björk.flac"), data, 0600)
	e := newRbxEnv(t, map[uint32]string{1: "/Users/dj/Music/Björk.flac"})
	m := RemoteMount{ID: "smb", Name: "Music", Protocol: "smb", Host: smb.Host, Port: smb.SlowProxy(t), RemotePath: "music", Username: smb.User, Password: smb.Password, MountPoint: filepath.Join(e.s.mediaRoot(), "share"), Enabled: true}
	b, _ := json.Marshal([]RemoteMount{m})
	os.WriteFile(e.s.mountsConfigPath(), b, 0600)
	oldRoom := rbxRoom
	rbxRoom = func(*syscall.Statfs_t, int64) int64 { return 1 << 30 }
	t.Cleanup(func() { rbxRoom = oldRoom })
	rbxNFS = func(net.IP) *prolink.NFSClient { t.Error("SMB should precede NFS"); return oldNFSForSMB() }
	ids, names := e.trackIDs(t, "Tracks")
	if strings.Join(names, "|") != "First|Second" {
		t.Fatalf("unexpected notice: %v", names)
	}
	id := ids["First"]
	tidalGet(t, e.base, "/v1/tracks/"+strconv.FormatInt(id, 10)+"/playbackinfopostpaywall", 200, nil)
	e.s.tidal.mu.Lock()
	ref := e.s.tidal.tracks[id]
	e.s.tidal.mu.Unlock()
	cp := rbxRunning(ref.file)
	if cp == nil {
		t.Fatal("expected a copy still arriving")
	}
	// Another load joins exactly the same rbxCopy.
	if _, err := e.s.tidal.rbxPrepare(context.Background(), id, ref); err != nil || rbxRunning(ref.file) != cp {
		t.Fatalf("copy not shared: %v", err)
	}
	req, _ := http.NewRequest("GET", e.base+"/v1/stream/"+strconv.FormatInt(id, 10)+".flac", nil)
	req.Header.Set("Range", "bytes=2-63")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != 206 || !bytes.Equal(got, data[2:64]) {
		t.Fatalf("Range: %d %q %v", resp.StatusCode, got, err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for rbxRunning(ref.file) != nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	got, err = os.ReadFile(ref.file)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("cache: %d bytes %v", len(got), err)
	}
	resolved, _ := filepath.EvalSymlinks(ref.file)
	if !strings.HasPrefix(resolved, filepath.Join(e.vol, rbxCacheDir)+"/") {
		t.Fatalf("wrong cache: %s", ref.file)
	}
	// The kernel-mounted path remains first even with an invalid SMB password.
	os.MkdirAll(m.MountPoint, 0700)
	os.WriteFile(filepath.Join(m.MountPoint, "Björk.flac"), data[:20], 0600)
	mountedAt = func(p string) bool { return p == m.MountPoint }
	if file, _, _, err := e.s.tidal.rbxFind(context.Background(), "127.0.0.1", "/Users/dj/Music/Björk.flac"); err != nil || file != filepath.Join(m.MountPoint, "Björk.flac") {
		t.Fatalf("kernel precedence: %s %v", file, err)
	}
}

func oldNFSForSMB() *prolink.NFSClient {
	return &prolink.NFSClient{IP: net.IPv4(127, 0, 0, 1), Portmap: 1, Timeout: time.Millisecond}
}

func TestSMBRekordboxWrongPasswordNotice(t *testing.T) {
	smb := testsmb.Start(t)
	e := newRbxEnv(t, map[uint32]string{1: "/Users/dj/Music/a.mp3"})
	m := RemoteMount{Protocol: "smb", Host: smb.Host, Port: smb.Port, RemotePath: "music", Username: smb.User, Password: "wrong", Enabled: true}
	b, _ := json.Marshal([]RemoteMount{m})
	os.WriteFile(e.s.mountsConfigPath(), b, 0600)
	start := time.Now()
	ids, names := e.trackIDs(t, "Tracks")
	if !strings.Contains(strings.Join(names, "|"), "does not share files with EngineX") {
		t.Fatalf("missing notice: %v", names)
	}
	body := tidalGet(t, e.base, "/v1/tracks/"+strconv.FormatInt(ids["First"], 10)+"/playbackinfopostpaywall", 502, nil)
	if !strings.Contains(string(body), "does not share files with EngineX") || time.Since(start) > 7*time.Second {
		t.Fatalf("wrong password: %s after %s", body, time.Since(start))
	}
}

func TestSMBRekordboxServerStopsMidCopy(t *testing.T) {
	smb := testsmb.Start(t)
	f, err := os.Create(filepath.Join(smb.Root, "a.flac"))
	if err != nil {
		t.Fatal(err)
	}
	f.Truncate(64 << 20)
	f.Close()
	e := newRbxEnv(t, map[uint32]string{1: "/Users/dj/Music/a.flac"})
	m := RemoteMount{Protocol: "smb", Host: smb.Host, Port: smb.SlowProxy(t), RemotePath: "music", Username: smb.User, Password: smb.Password, Enabled: true}
	b, _ := json.Marshal([]RemoteMount{m})
	os.WriteFile(e.s.mountsConfigPath(), b, 0600)
	oldRoom := rbxRoom
	rbxRoom = func(*syscall.Statfs_t, int64) int64 { return 1 << 30 }
	t.Cleanup(func() { rbxRoom = oldRoom })
	ids, _ := e.trackIDs(t, "Tracks")
	id := ids["First"]
	tidalGet(t, e.base, "/v1/tracks/"+strconv.FormatInt(id, 10)+"/playbackinfopostpaywall", 200, nil)
	e.s.tidal.mu.Lock()
	ref := e.s.tidal.tracks[id]
	e.s.tidal.mu.Unlock()
	cp := rbxRunning(ref.file)
	if cp == nil {
		t.Fatal("no copy in progress")
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		cp.mu.Lock()
		got := cp.got
		cp.mu.Unlock()
		if got > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	smb.Stop()
	start := time.Now()
	for time.Since(start) < 5*time.Second {
		cp.mu.Lock()
		done, err := cp.done, cp.err
		cp.mu.Unlock()
		if done {
			if err == nil {
				t.Fatal("stopped copy succeeded")
			}
			if _, err := os.Stat(ref.file); !os.IsNotExist(err) {
				t.Fatal("partial cache published")
			}
			_, err = e.s.tidal.rbxPrepare(context.Background(), id, ref)
			if err == nil || !strings.Contains(err.Error(), "does not share files with EngineX") || time.Since(start) > 5*time.Second {
				t.Fatalf("reload after interrupted copy: %v after %s", err, time.Since(start))
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("stopped SMB server left a stuck copy")
}
