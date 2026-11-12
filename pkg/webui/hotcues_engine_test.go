package webui

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/dbhealer"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

// hotCueLibrary is a drive whose track 10 is a 48 kHz file with Hot Cue 1 at
// sample 480000 (10 s) and the other seven unset.
func hotCueLibrary(t *testing.T) (mediaRoot, dbPath string, db *sql.DB) {
	t.Helper()
	mediaRoot = t.TempDir()
	dbDir := filepath.Join(mediaRoot, "CUES", "Engine Library", "Database2")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath = filepath.Join(dbDir, "m.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE PerformanceData (trackId INTEGER PRIMARY KEY, trackData BLOB, quickCues BLOB)`); err != nil {
		t.Fatal(err)
	}
	track := make([]byte, 44)
	binary.BigEndian.PutUint64(track, math.Float64bits(48000))
	binary.BigEndian.PutUint64(track[8:], 48000*300)
	var cues bytes.Buffer
	binary.Write(&cues, binary.BigEndian, uint64(8))
	for i := 0; i < 8; i++ {
		pos := -1.0
		if i == 0 {
			pos = 480000
		}
		cues.WriteByte(0)
		binary.Write(&cues, binary.BigEndian, pos)
		cues.Write([]byte{0, 0, 0, 0})
	}
	if _, err := db.Exec(`INSERT INTO PerformanceData VALUES (10, ?, ?)`, dbhealer.EncodeQCompress(track), dbhealer.EncodeQCompress(cues.Bytes())); err != nil {
		t.Fatal(err)
	}
	return mediaRoot, dbPath, db
}

func cueSamples(t *testing.T, db *sql.DB, index int) float64 {
	t.Helper()
	var blob []byte
	if err := db.QueryRow(`SELECT quickCues FROM PerformanceData WHERE trackId=10`).Scan(&blob); err != nil {
		t.Fatal(err)
	}
	raw, err := dbhealer.DecodeQCompress(blob)
	if err != nil {
		t.Fatal(err)
	}
	off := 8
	for i := 0; i < index; i++ {
		off += 1 + int(raw[off]) + 12
	}
	off += 1 + int(raw[off])
	return math.Float64frombits(binary.BigEndian.Uint64(raw[off:]))
}

func postCue(t *testing.T, url string, body map[string]any) map[string]any {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(url+"/v1/sc5000/hotcues", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("POST: %d", resp.StatusCode)
	}
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return out
}

// BH-GC2-1: Hot Cue positions were converted with 44100 Hz, so a 48 kHz cue at
// 10 s read as 10884 ms and a 10 s edit was written to sample 441000 (9.19 s).
func TestHotCuesUseTheTracksSampleRate(t *testing.T) {
	mediaRoot, _, db := hotCueLibrary(t)
	srv := httptest.NewServer(New(Config{AccessToken: "t", MediaRoot: mediaRoot, UIDir: t.TempDir(), StateDir: t.TempDir()}, nil))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/sc5000/hotcues?drive=CUES&trackId=10")
	if err != nil {
		t.Fatal(err)
	}
	var got hotCuesResponse
	json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if len(got.Slots) != 8 || math.Abs(got.Slots[0].PositionMS-10000) > 0.001 {
		t.Fatalf("cue 1 read as %+v", got.Slots)
	}
	postCue(t, srv.URL, map[string]any{"drive": "CUES", "trackId": 10, "index": 1, "label": "Drop", "color": "#E53935", "positionMs": 20000})
	if s := cueSamples(t, db, 1); s != 960000 {
		t.Fatalf("20 s at 48 kHz written as sample %v", s)
	}
	if s := cueSamples(t, db, 0); s != 480000 {
		t.Fatalf("untouched cue moved: %v", s)
	}
}

// BH-GC2-1: a Hot Cue edit wrote m.db while Engine had it open. It is kept
// (and shown) and reaches m.db before Engine next opens the library.
func TestHotCuesWaitWhileAnotherProcessHoldsTheLibrary(t *testing.T) {
	mediaRoot, dbPath, db := hotCueLibrary(t)
	f, err := os.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	engine := exec.Command("sleep", "60")
	engine.ExtraFiles = []*os.File{f}
	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}
	f.Close()
	defer func() { engine.Process.Kill(); engine.Wait() }()
	before := cueSamples(t, db, 1)
	srv := httptest.NewServer(New(Config{AccessToken: "t", MediaRoot: mediaRoot, UIDir: t.TempDir(), StateDir: t.TempDir()}, nil))
	defer srv.Close()
	out := postCue(t, srv.URL, map[string]any{"drive": "CUES", "trackId": 10, "index": 1, "label": "Drop", "color": "#E53935", "positionMs": 20000})
	if s := cueSamples(t, db, 1); s != before {
		t.Fatalf("m.db written while another process has it open: %v", s)
	}
	if out["pending"] != true {
		t.Fatalf("response does not say the edit waits: %v", out)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dbPath), "enginex-pending.json")); err != nil {
		t.Fatalf("edit not queued: %v", err)
	}
	resp, err := http.Get(srv.URL + "/v1/sc5000/hotcues?drive=CUES&trackId=10")
	if err != nil {
		t.Fatal(err)
	}
	var got hotCuesResponse
	json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if got.Slots[1].Label != "Drop" || got.Slots[1].PositionMS != 20000 {
		t.Fatalf("queued edit not shown: %+v", got.Slots[1])
	}
	// Engine closed the library (the drive is inserted again): the guardian applies it.
	engine.Process.Kill()
	engine.Wait()
	if n, err := enginedb.ApplyPending(context.Background(), filepath.Join(mediaRoot, "CUES")); err != nil || n != 1 {
		t.Fatalf("apply: %d %v", n, err)
	}
	if s := cueSamples(t, db, 1); s != 960000 {
		t.Fatalf("queued cue written as sample %v", s)
	}
}
