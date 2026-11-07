package webui

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/prolink"
)

func TestListeningSockets(t *testing.T) {
	dir := t.TempDir()
	hdr := "  sl  local_address rem_address   st tx_queue rx_queue\n"
	os.WriteFile(filepath.Join(dir, "tcp"), []byte(hdr+
		"   0: 00000000:30EB 00000000:0000 0A 00000000:00000000\n"+ // 12523 LISTEN
		"   1: 0100007F:1F90 0100007F:D431 01 00000000:00000000\n"), 0o644) // 8080 ESTABLISHED
	os.WriteFile(filepath.Join(dir, "udp6"), []byte(hdr+
		"   0: 00000000000000000000000000000000:C350 00000000000000000000000000000000:0000 07 00000000:00000000\n"), 0o644)
	got := listeningSockets(dir)
	if !got["tcp/12523"] || !got["udp/50000"] || got["tcp/8080"] || len(got) != 2 {
		t.Fatalf("got %v", got)
	}
}

func TestNFSPortProbe(t *testing.T) {
	nfs := &prolink.NFSServer{Root: t.TempDir()}
	if err := nfs.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	defer nfs.Close()
	port, err := (&prolink.NFSClient{IP: net.IPv4(127, 0, 0, 1), Portmap: nfs.PortmapPort()}).NFSPort(context.Background())
	if err != nil || port <= 0 {
		t.Fatalf("port %d, %v", port, err)
	}
}

func TestProlinkDiag(t *testing.T) {
	ui := t.TempDir()
	os.WriteFile(filepath.Join(ui, "settings.conf"), []byte("prolink_share=1\n"), 0o644)
	procNetDir = t.TempDir() // nothing listens
	t.Cleanup(func() { procNetDir = "/proc/net" })
	srv := New(Config{StateDir: t.TempDir(), UIDir: ui, AccessToken: "secret-access-code"}, nil)
	defer srv.Close()
	t.Setenv("SC5000_PROLINK_STATE", filepath.Join(t.TempDir(), "none.json"))
	prolinkMu.Lock()
	postedDevices[srv] = map[string]prolink.Device{"3": {Number: 3, Name: "CDJ-3000", Address: "192.0.2.33", LastSeen: time.Now(), EffectiveBPM: 128, Playing: true, Master: true}}
	prolinkMu.Unlock()
	defer func() { prolinkMu.Lock(); delete(postedDevices, srv); prolinkMu.Unlock() }()

	get := func(path, remote, auth string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = remote
		if auth != "" {
			req.Header.Set("Authorization", "Bearer "+auth)
		}
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}
	if rec := get("/api/prolink/diag", "192.0.2.9:1", ""); rec.Code != 401 {
		t.Fatalf("console route without login: %d", rec.Code)
	}
	if rec := get("/v1/sc5000/prolink/diag", "192.0.2.9:1", ""); rec.Code != 403 {
		t.Fatalf("gateway route from the LAN: %d", rec.Code)
	}

	rec := get("/api/prolink/diag", "192.0.2.9:1", "secret-access-code")
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var d prolinkDiag
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if !d.Settings.ProlinkShare || d.Settings.ProlinkExport || len(d.Peers) != 1 || d.Peers[0].Name != "CDJ-3000" || d.Peers[0].BPM != 128 {
		t.Fatalf("diag %+v", d)
	}
	if d.Peers[0].NFS == "" {
		t.Fatal("the CDJ's NFS server was not probed")
	}
	want := map[string]string{"udp 50000": "FAIL", "Pro DJ Link Export to CDJ (Beta)": "WARN", "Pro DJ Link Library": "PASS", "Peer 3 CDJ-3000": "FAIL"}
	for _, c := range d.Checks {
		if lvl, ok := want[c.Item]; ok {
			if c.Level != lvl {
				t.Errorf("%s: %s, want %s (%s)", c.Item, c.Level, lvl, c.Detail)
			}
			delete(want, c.Item)
		}
		if c.Item == "udp 111" {
			t.Error("export ports are checked while the export is off")
		}
	}
	if len(want) != 0 {
		t.Errorf("missing checks %v in %+v", want, d.Checks)
	}

	rec = get("/v1/sc5000/prolink/diag?format=text", "127.0.0.1:1", "")
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, "### EngineX OS Pro DJ Link diagnostic") || !strings.Contains(body, "| 3 | CDJ-3000 | 192.0.2.33 |") {
		t.Fatalf("text report %d:\n%s", rec.Code, body)
	}
	if strings.Contains(body, "secret-access-code") || strings.Contains(get("/api/prolink/diag", "192.0.2.9:1", "secret-access-code").Body.String(), "secret-access-code") {
		t.Fatal("the report leaks the console access code")
	}
}
