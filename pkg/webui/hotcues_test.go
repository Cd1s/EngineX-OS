package webui

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"math"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/dbhealer"
)

func TestHotCuesGetAndPost(t *testing.T) {
	mediaRoot := t.TempDir()
	driveName := "USB_DRIVE"
	driveDir := filepath.Join(mediaRoot, driveName)
	dbDir := filepath.Join(driveDir, "Engine Library", "Database2")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatal(err)
	}

	ui := New(Config{
		AccessToken: "test-token",
		MediaRoot:   mediaRoot,
		UIDir:       t.TempDir(),
		StateDir:    t.TempDir(),
	}, nil)
	srv := httptest.NewServer(ui)
	defer srv.Close()

	client := srv.Client()

	// 1. Initial GET on an empty drive: should return 8 default slots & palette
	getURL := srv.URL + "/v1/sc5000/hotcues?drive=" + driveName + "&trackId=123"
	resp, err := client.Get(getURL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var getRes hotCuesResponse
	if err := json.NewDecoder(resp.Body).Decode(&getRes); err != nil {
		t.Fatal(err)
	}
	if getRes.Drive != driveName || getRes.TrackID != 123 {
		t.Fatalf("response mismatch: drive=%s, trackId=%d", getRes.Drive, getRes.TrackID)
	}
	if len(getRes.Cues) != 8 || len(getRes.Slots) != 8 {
		t.Fatalf("expected 8 slots, got cues=%d slots=%d", len(getRes.Cues), len(getRes.Slots))
	}
	if len(getRes.Palette) != 16 {
		t.Fatalf("expected 16 palette colors, got %d", len(getRes.Palette))
	}
	for i, slot := range getRes.Cues {
		if slot.Index != i || slot.Label != "" || slot.Color != "" {
			t.Fatalf("slot %d not empty: %+v", i, slot)
		}
	}

	// 2. POST update slot 0: label="Drop", color="#E53935", positionMs=15000
	pos := 15000.0
	postBody, _ := json.Marshal(map[string]any{
		"drive":      driveName,
		"trackId":    123,
		"index":      0,
		"label":      "Drop",
		"color":      "#E53935",
		"positionMs": pos,
	})
	postResp, err := client.Post(srv.URL+"/v1/sc5000/hotcues", "application/json", bytes.NewReader(postBody))
	if err != nil {
		t.Fatal(err)
	}
	defer postResp.Body.Close()
	if postResp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", postResp.StatusCode)
	}
	var postRes hotCuesResponse
	if err := json.NewDecoder(postResp.Body).Decode(&postRes); err != nil {
		t.Fatal(err)
	}
	if postRes.Cues[0].Label != "Drop" || postRes.Cues[0].Color != "#E53935" || postRes.Cues[0].PositionMS != 15000 {
		t.Fatalf("post response slot 0 mismatch: %+v", postRes.Cues[0])
	}

	// 3. POST update slot 3: label="Break", color="#00ACC1"
	postBody2, _ := json.Marshal(map[string]any{
		"drive":   driveName,
		"trackId": 123,
		"index":   3,
		"label":   "Break",
		"color":   "#00ACC1",
	})
	postResp2, err := client.Post(srv.URL+"/v1/sc5000/hotcues", "application/json", bytes.NewReader(postBody2))
	if err != nil {
		t.Fatal(err)
	}
	defer postResp2.Body.Close()
	if postResp2.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", postResp2.StatusCode)
	}

	// 4. Verify persistence in enginex_hotcues.json
	jsonPath := filepath.Join(dbDir, "enginex_hotcues.json")
	jsonBytes, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatalf("file not created: %v", err)
	}
	var diskMap map[string][]HotCueSlot
	if err := json.Unmarshal(jsonBytes, &diskMap); err != nil {
		t.Fatalf("invalid json on disk: %v", err)
	}
	diskSlots, ok := diskMap["123"]
	if !ok || len(diskSlots) != 8 {
		t.Fatalf("track 123 missing or wrong slot count on disk: %+v", diskMap)
	}
	if diskSlots[0].Label != "Drop" || diskSlots[0].Color != "#E53935" {
		t.Fatalf("slot 0 on disk mismatch: %+v", diskSlots[0])
	}
	if diskSlots[3].Label != "Break" || diskSlots[3].Color != "#00ACC1" {
		t.Fatalf("slot 3 on disk mismatch: %+v", diskSlots[3])
	}

	// 5. GET again and verify updated slots
	resp2, err := client.Get(getURL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var getRes2 hotCuesResponse
	json.NewDecoder(resp2.Body).Decode(&getRes2)
	if getRes2.Cues[0].Label != "Drop" || getRes2.Cues[0].Color != "#E53935" {
		t.Fatalf("get slot 0 mismatch: %+v", getRes2.Cues[0])
	}
	if getRes2.Cues[3].Label != "Break" || getRes2.Cues[3].Color != "#00ACC1" {
		t.Fatalf("get slot 3 mismatch: %+v", getRes2.Cues[3])
	}
}

func TestHotCuesValidationAndSecurity(t *testing.T) {
	mediaRoot := t.TempDir()
	driveName := "VALID_DRIVE"
	driveDir := filepath.Join(mediaRoot, driveName)
	os.MkdirAll(filepath.Join(driveDir, "Engine Library", "Database2"), 0o755)

	ui := New(Config{
		AccessToken: "test-token",
		MediaRoot:   mediaRoot,
		UIDir:       t.TempDir(),
		StateDir:    t.TempDir(),
	}, nil)
	srv := httptest.NewServer(ui)
	defer srv.Close()

	client := srv.Client()

	// 1. Path traversal attacks
	traversals := []string{
		"../etc",
		"..",
		"../../",
		"/root",
		"foo/bar",
		"drive\\escape",
	}
	for _, badDrive := range traversals {
		resp, _ := client.Get(srv.URL + "/v1/sc5000/hotcues?drive=" + badDrive + "&trackId=1")
		if resp.StatusCode != 400 {
			t.Errorf("expected 400 for GET traversal %q, got %d", badDrive, resp.StatusCode)
		}
		resp.Body.Close()

		postBody, _ := json.Marshal(map[string]any{
			"drive":   badDrive,
			"trackId": 1,
			"index":   0,
			"label":   "Bad",
			"color":   "#E53935",
		})
		pResp, _ := client.Post(srv.URL+"/v1/sc5000/hotcues", "application/json", bytes.NewReader(postBody))
		if pResp.StatusCode != 400 {
			t.Errorf("expected 400 for POST traversal %q, got %d", badDrive, pResp.StatusCode)
		}
		pResp.Body.Close()
	}

	// 2. Out of bounds index
	for _, badIndex := range []int{-1, 8, 99} {
		postBody, _ := json.Marshal(map[string]any{
			"drive":   driveName,
			"trackId": 1,
			"index":   badIndex,
			"label":   "Bad",
			"color":   "#E53935",
		})
		pResp, _ := client.Post(srv.URL+"/v1/sc5000/hotcues", "application/json", bytes.NewReader(postBody))
		if pResp.StatusCode != 400 {
			t.Errorf("expected 400 for index %d, got %d", badIndex, pResp.StatusCode)
		}
		pResp.Body.Close()
	}

	// 3. Invalid color format
	for _, badColor := range []string{"red", "#12345", "#1234567", "#GGGGGG", "E53935", "#12"} {
		postBody, _ := json.Marshal(map[string]any{
			"drive":   driveName,
			"trackId": 1,
			"index":   0,
			"label":   "Bad",
			"color":   badColor,
		})
		pResp, _ := client.Post(srv.URL+"/v1/sc5000/hotcues", "application/json", bytes.NewReader(postBody))
		if pResp.StatusCode != 400 {
			t.Errorf("expected 400 for color %q, got %d", badColor, pResp.StatusCode)
		}
		pResp.Body.Close()
	}

	// 4. Invalid trackId
	for _, badTrackID := range []any{-1, 0, "not-a-number"} {
		postBody, _ := json.Marshal(map[string]any{
			"drive":   driveName,
			"trackId": badTrackID,
			"index":   0,
			"label":   "Bad",
			"color":   "#E53935",
		})
		pResp, _ := client.Post(srv.URL+"/v1/sc5000/hotcues", "application/json", bytes.NewReader(postBody))
		if pResp.StatusCode != 400 {
			t.Errorf("expected 400 for trackId %v, got %d", badTrackID, pResp.StatusCode)
		}
		pResp.Body.Close()
	}
}

func TestHotCuesSyncWithMDB(t *testing.T) {
	mediaRoot := t.TempDir()
	driveName := "SYNC_DRIVE"
	driveDir := filepath.Join(mediaRoot, driveName)
	dbDir := filepath.Join(driveDir, "Engine Library", "Database2")
	os.MkdirAll(dbDir, 0o755)

	dbPath := filepath.Join(dbDir, "m.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	_, err = db.Exec(`
		CREATE TABLE PerformanceData (
			trackId INTEGER PRIMARY KEY,
			quickCues BLOB
		);
	`)
	if err != nil {
		t.Fatal(err)
	}

	// Create an initial raw quickCues blob with 8 slots
	var raw bytes.Buffer
	_ = binaryWriteBigEndian(&raw, uint64(8))
	for i := 0; i < 8; i++ {
		raw.WriteByte(0)                     // label length 0
		_ = binaryWriteBigEndian(&raw, -1.0) // position
		raw.Write([]byte{0, 0, 0, 0})        // ARGB
	}
	compressed := dbhealer.EncodeQCompress(raw.Bytes())
	_, err = db.Exec("INSERT INTO PerformanceData (trackId, quickCues) VALUES (10, ?)", compressed)
	if err != nil {
		t.Fatal(err)
	}

	ui := New(Config{
		AccessToken: "test-token",
		MediaRoot:   mediaRoot,
		UIDir:       t.TempDir(),
		StateDir:    t.TempDir(),
	}, nil)
	srv := httptest.NewServer(ui)
	defer srv.Close()

	client := srv.Client()

	// Update slot 1 via POST
	postBody, _ := json.Marshal(map[string]any{
		"drive":   driveName,
		"trackId": 10,
		"index":   1,
		"label":   "Chorus",
		"color":   "#FB8C00",
	})
	resp, err := client.Post(srv.URL+"/v1/sc5000/hotcues", "application/json", bytes.NewReader(postBody))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	// Verify that m.db PerformanceData was updated
	var updatedBlob []byte
	err = db.QueryRow("SELECT quickCues FROM PerformanceData WHERE trackId = 10").Scan(&updatedBlob)
	if err != nil {
		t.Fatal(err)
	}
	decomp, err := dbhealer.DecodeQCompress(updatedBlob)
	if err != nil {
		t.Fatal(err)
	}

	// Decode decompressed slots to verify slot 1
	slots, ok := readHotCuesFromDB(dbPath, 10)
	if !ok || len(slots) != 8 {
		t.Fatalf("readHotCuesFromDB failed: ok=%v, slots=%+v, decompLen=%d", ok, slots, len(decomp))
	}
	if slots[1].Label != "Chorus" || slots[1].Color != "#FB8C00" {
		t.Fatalf("synced slot 1 mismatch in m.db: %+v", slots[1])
	}
}

func binaryWriteBigEndian(buf *bytes.Buffer, v any) error {
	switch val := v.(type) {
	case uint64:
		var b [8]byte
		b[0] = byte(val >> 56)
		b[1] = byte(val >> 48)
		b[2] = byte(val >> 40)
		b[3] = byte(val >> 32)
		b[4] = byte(val >> 24)
		b[5] = byte(val >> 16)
		b[6] = byte(val >> 8)
		b[7] = byte(val)
		buf.Write(b[:])
	case float64:
		var b [8]byte
		bits := math.Float64bits(val)
		b[0] = byte(bits >> 56)
		b[1] = byte(bits >> 48)
		b[2] = byte(bits >> 40)
		b[3] = byte(bits >> 32)
		b[4] = byte(bits >> 24)
		b[5] = byte(bits >> 16)
		b[6] = byte(bits >> 8)
		b[7] = byte(bits)
		buf.Write(b[:])
	}
	return nil
}
