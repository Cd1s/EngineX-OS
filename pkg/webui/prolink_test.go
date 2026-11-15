package webui

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/prolink"
)

func TestProlinkStatePath(t *testing.T) {
	stateDir := t.TempDir()
	srv := New(Config{StateDir: stateDir}, nil)

	// 1. Initially no prolink.json exists and no env set -> returns /tmp/enginex-prolink.json
	t.Setenv("ENGINEX_PROLINK_STATE", "")
	if p := srv.ProlinkStatePath(); p != "/tmp/enginex-prolink.json" {
		t.Errorf("got %q, want /tmp/enginex-prolink.json", p)
	}

	// 2. ENGINEX_PROLINK_STATE env set and no file in StateDir -> returns env value
	custom := filepath.Join(stateDir, "custom-env.json")
	t.Setenv("ENGINEX_PROLINK_STATE", custom)
	if p := srv.ProlinkStatePath(); p != custom {
		t.Errorf("got %q, want %q", p, custom)
	}

	// 3. When prolink.json exists in StateDir -> returns that file path
	stateFile := filepath.Join(stateDir, "prolink.json")
	if err := os.WriteFile(stateFile, []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	if p := srv.ProlinkStatePath(); p != stateFile {
		t.Errorf("got %q, want %q", p, stateFile)
	}
}

func TestProlinkGetEmpty(t *testing.T) {
	stateDir := t.TempDir()
	srv := New(Config{StateDir: stateDir}, nil)

	req := httptest.NewRequest(http.MethodGet, "/v1/enginex/prolink", nil)
	rec := httptest.NewRecorder()
	srv.prolinkGet(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("got status %d, want 200", rec.Code)
	}
	var resp struct {
		Enabled   bool                      `json:"enabled"`
		UpdatedAt time.Time                 `json:"updatedAt"`
		Devices   []prolink.Device          `json:"devices"`
		Decks     map[string]prolink.Device `json:"decks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if !resp.Enabled {
		t.Error("expected enabled: true")
	}
	if len(resp.Devices) != 0 {
		t.Errorf("got %d devices, want 0", len(resp.Devices))
	}
	if len(resp.Decks) != 0 {
		t.Errorf("got %d decks, want 0", len(resp.Decks))
	}
}

func TestProlinkPostSingleDeviceAndGet(t *testing.T) {
	stateDir := t.TempDir()
	srv := New(Config{StateDir: stateDir}, nil)

	dev := prolink.Device{
		Number:   1,
		Name:     "CDJ-3000",
		Address:  "192.168.1.101",
		BPM:      128.0,
		Playing:  true,
		Title:    "Club Track",
		Artist:   "DJ Producer",
		LastSeen: time.Now(),
	}
	body, _ := json.Marshal(dev)

	postReq := httptest.NewRequest(http.MethodPost, "/v1/enginex/prolink", bytes.NewReader(body))
	postRec := httptest.NewRecorder()
	srv.prolinkPost(postRec, postReq)

	if postRec.Code != http.StatusOK {
		t.Fatalf("got status %d, want 200", postRec.Code)
	}
	var postResp struct {
		Enabled   bool                      `json:"enabled"`
		UpdatedAt time.Time                 `json:"updatedAt"`
		Devices   []prolink.Device          `json:"devices"`
		Decks     map[string]prolink.Device `json:"decks"`
	}
	if err := json.Unmarshal(postRec.Body.Bytes(), &postResp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if !postResp.Enabled {
		t.Error("expected enabled: true")
	}
	if len(postResp.Devices) != 1 {
		t.Fatalf("got %d devices, want 1", len(postResp.Devices))
	}
	// Deck 3 should mirror Deck 1 when missing
	if postResp.Decks["1"].Number != 1 || postResp.Decks["3"].Number != 1 {
		t.Errorf("deck 1/3 mapping mismatch: %+v", postResp.Decks)
	}
	if postResp.Decks["1"].Title != "Club Track" {
		t.Errorf("got title %q, want 'Club Track'", postResp.Decks["1"].Title)
	}

	// Verify state file was written atomically
	stateFile := filepath.Join(stateDir, "prolink.json")
	if _, err := os.Stat(stateFile); err != nil {
		t.Fatalf("state file was not created: %v", err)
	}

	// Test GET endpoint returns the same snapshot
	getReq := httptest.NewRequest(http.MethodGet, "/v1/enginex/prolink", nil)
	getRec := httptest.NewRecorder()
	srv.prolinkGet(getRec, getReq)

	if getRec.Code != http.StatusOK {
		t.Fatalf("get got status %d, want 200", getRec.Code)
	}
	var getResp struct {
		Enabled bool                      `json:"enabled"`
		Devices []prolink.Device          `json:"devices"`
		Decks   map[string]prolink.Device `json:"decks"`
	}
	if err := json.Unmarshal(getRec.Body.Bytes(), &getResp); err != nil {
		t.Fatal(err)
	}
	if len(getResp.Devices) != 1 || getResp.Decks["1"].Number != 1 {
		t.Errorf("unexpected get response: %+v", getResp)
	}
}

func TestProlinkPostDeviceListAndDecksMapping(t *testing.T) {
	stateDir := t.TempDir()
	srv := New(Config{StateDir: stateDir}, nil)

	payload := struct {
		Devices []prolink.Device `json:"devices"`
	}{
		Devices: []prolink.Device{
			{Number: 1, Name: "CDJ-3000", BPM: 128.0, LastSeen: time.Now()},
			{Number: 2, Name: "CDJ-3000", BPM: 125.0, LastSeen: time.Now()},
		},
	}
	body, _ := json.Marshal(payload)

	postReq := httptest.NewRequest(http.MethodPost, "/v1/enginex/prolink", bytes.NewReader(body))
	postRec := httptest.NewRecorder()
	srv.prolinkPost(postRec, postReq)

	if postRec.Code != http.StatusOK {
		t.Fatalf("post got status %d, want 200", postRec.Code)
	}
	var resp struct {
		Devices []prolink.Device          `json:"devices"`
		Decks   map[string]prolink.Device `json:"decks"`
	}
	if err := json.Unmarshal(postRec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Devices) != 2 {
		t.Fatalf("got %d devices, want 2", len(resp.Devices))
	}
	// With 1 & 2 present: 3 mirrors 1, 4 mirrors 2
	if resp.Decks["1"].Number != 1 || resp.Decks["2"].Number != 2 {
		t.Errorf("decks 1 & 2 mismatch: %+v", resp.Decks)
	}
	if resp.Decks["3"].Number != 1 || resp.Decks["4"].Number != 2 {
		t.Errorf("decks 3 & 4 mirroring mismatch: %+v", resp.Decks)
	}
}

func TestProlinkStateFileMerging(t *testing.T) {
	stateDir := t.TempDir()
	srv := New(Config{StateDir: stateDir}, nil)

	// Pre-populate state file
	snap := prolink.Snapshot{
		UpdatedAt: time.Now(),
		Devices: []prolink.Device{
			{Number: 1, Title: "Existing Song", Artist: "Existing Artist", BPM: 120.0, LastSeen: time.Now()},
		},
		Decks: map[string]prolink.Device{
			"1": {Number: 1, Title: "Existing Song", Artist: "Existing Artist", BPM: 120.0, LastSeen: time.Now()},
		},
	}
	data, _ := json.Marshal(snap)
	_ = os.WriteFile(filepath.Join(stateDir, "prolink.json"), data, 0644)

	// Post an update with only real-time fields
	update := prolink.Device{
		Number:   1,
		BPM:      128.0,
		Playing:  true,
		LastSeen: time.Now(),
	}
	body, _ := json.Marshal(update)
	postReq := httptest.NewRequest(http.MethodPost, "/v1/enginex/prolink", bytes.NewReader(body))
	postRec := httptest.NewRecorder()
	srv.prolinkPost(postRec, postReq)

	if postRec.Code != http.StatusOK {
		t.Fatalf("post got status %d, want 200", postRec.Code)
	}
	var resp struct {
		Devices []prolink.Device          `json:"devices"`
		Decks   map[string]prolink.Device `json:"decks"`
	}
	if err := json.Unmarshal(postRec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Devices) != 1 {
		t.Fatalf("got %d devices, want 1", len(resp.Devices))
	}
	d := resp.Decks["1"]
	if d.BPM != 128.0 || !d.Playing {
		t.Errorf("updated fields not reflected: %+v", d)
	}
	if d.Title != "Existing Song" || d.Artist != "Existing Artist" {
		t.Errorf("pre-existing metadata lost in merge: %+v", d)
	}
}

// #59: the media share toggle is routed on the gateway and shared with the UI settings.
func TestProlinkShareToggle(t *testing.T) {
	t.Setenv("ENGINEX_PROLINK_STATE", filepath.Join(t.TempDir(), "none.json"))
	ui := New(Config{AccessToken: "c", UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	srv := httptest.NewServer(ui)
	defer srv.Close()
	get := func(method, query string) (int, prolinkResponse) {
		req, _ := http.NewRequest(method, srv.URL+"/v1/enginex/prolink"+query, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var v prolinkResponse
		json.NewDecoder(resp.Body).Decode(&v)
		return resp.StatusCode, v
	}
	if code, v := get("GET", ""); code != 200 || v.Share {
		t.Fatalf("%d %+v", code, v)
	}
	if code, v := get("POST", "?share=1"); code != 200 || !v.Share {
		t.Fatalf("%d %+v", code, v)
	}
	if code, v := uiPost(t, srv.URL, ""); code != 200 || !v.ProlinkShare {
		t.Fatalf("%d %+v", code, v)
	}
	if code, _ := get("POST", "?share=yes"); code != 400 {
		t.Fatalf("bad value: %d", code)
	}
}

// A CDJ back at pitch 0 or cued to 0 ms is followed: zero is a value, not "unknown".
func TestMergeDeviceKeepsZeroPitchAndCue(t *testing.T) {
	d := prolink.Device{PacketType: 0x0a, Pitch: 20.83, PlayPositionMS: 5000}
	d = mergeDevice(d, prolink.Device{PacketType: 0x0a, Pitch: 0})
	if d.Pitch != 0 {
		t.Fatalf("status pitch 0 ignored: %v", d.Pitch)
	}
	d.Pitch = 3
	if d = mergeDevice(d, prolink.Device{PacketType: 6}); d.Pitch != 3 {
		t.Fatalf("keepalive cleared the pitch: %v", d.Pitch)
	}
	if d = mergeDevice(d, prolink.Device{PacketType: 0x0b, PrecisePosition: true}); d.PlayPositionMS != 0 || d.Pitch != 0 {
		t.Fatalf("cue to 0 ms ignored: %+v", d)
	}
}
