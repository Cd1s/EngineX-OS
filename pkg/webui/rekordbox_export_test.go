package webui

import (
	"bytes"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/dbhealer"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	_ "modernc.org/sqlite"
)

func setupWebUITestLibrary(t *testing.T, mediaRoot, driveName string) string {
	driveDir := filepath.Join(mediaRoot, driveName)
	dbDir := filepath.Join(driveDir, "Engine Library", "Database2")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dbDir, "m.db")
	u := url.URL{Scheme: "file", Path: dbPath}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	_, err = db.Exec(`
		CREATE TABLE Track (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			pdbImportKey INTEGER,
			title TEXT,
			artist TEXT,
			album TEXT,
			genre TEXT,
			comment TEXT,
			composer TEXT,
			remixer TEXT,
			label TEXT,
			year INTEGER,
			key INTEGER,
			rating INTEGER,
			bpmAnalyzed REAL,
			bpm INTEGER,
			length INTEGER,
			bitrate INTEGER,
			fileBytes INTEGER,
			filename TEXT,
			path TEXT,
			fileType TEXT
		);
		CREATE TABLE PerformanceData (
			trackId INTEGER PRIMARY KEY,
			trackData BLOB,
			beatData BLOB,
			quickCues BLOB,
			loops BLOB
		);
		CREATE TABLE Playlist (id INTEGER PRIMARY KEY, title TEXT, parentListId INTEGER);
		CREATE TABLE PlaylistEntity (id INTEGER PRIMARY KEY, listId INTEGER, trackId INTEGER);
	`)
	if err != nil {
		t.Fatal(err)
	}

	var td [44]byte
	binary.BigEndian.PutUint64(td[:8], math.Float64bits(44100.0))
	binary.BigEndian.PutUint64(td[8:16], 44100*120)
	tdEnc, _ := enginedb.QCompress(td[:])

	var qc bytes.Buffer
	var cnt [8]byte
	binary.BigEndian.PutUint64(cnt[:], 8)
	qc.Write(cnt[:])
	// Hot cue 0 (pad 1)
	qc.WriteByte(byte(len("Vocal")))
	qc.Write([]byte("Vocal"))
	var pos0 [8]byte
	binary.BigEndian.PutUint64(pos0[:], math.Float64bits(44100.0*5.0))
	qc.Write(pos0[:])
	qc.Write([]byte{255, 255, 128, 0}) // Orange
	for i := 1; i < 8; i++ {
		qc.WriteByte(0)
		var unset [8]byte
		binary.BigEndian.PutUint64(unset[:], math.Float64bits(-1.0))
		qc.Write(unset[:])
		qc.Write([]byte{0, 0, 0, 0})
	}
	var mainCue [8]byte
	binary.BigEndian.PutUint64(mainCue[:], math.Float64bits(0.0))
	qc.Write(mainCue[:])
	qc.Write(make([]byte, 10))
	qcEnc := dbhealer.EncodeQCompress(qc.Bytes())

	_, err = db.Exec(`
		INSERT INTO Track (id, pdbImportKey, title, artist, length, bpmAnalyzed, path)
		VALUES (1, 101, 'Vocal Tune', 'Artist A', 120, 124.0, '../Music/vocal.mp3');
		INSERT INTO PerformanceData (trackId, trackData, quickCues)
		VALUES (1, ?, ?);
		INSERT INTO Playlist (id, title, parentListId) VALUES (1, 'Setlist', NULL);
		INSERT INTO PlaylistEntity (id, listId, trackId) VALUES (1, 1, 1);
	`, tdEnc, qcEnc)
	if err != nil {
		t.Fatal(err)
	}

	return driveDir
}

func TestRekordboxExportWebUI(t *testing.T) {
	mediaRoot := t.TempDir()
	driveDir := setupWebUITestLibrary(t, mediaRoot, "USB_DRIVE")

	s := New(Config{
		MediaRoot:   mediaRoot,
		AccessToken: "test-token",
		UIDir:       t.TempDir(),
		StateDir:    t.TempDir(),
	}, nil)
	defer s.Close()

	// 1. GET /api/rekordbox/export?drive=USB_DRIVE (download)
	req := httptest.NewRequest(http.MethodGet, "/api/rekordbox/export?drive=USB_DRIVE&os=mac", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/rekordbox/export code = %d, want 200, body: %s", w.Code, w.Body.String())
	}
	contentType := w.Header().Get("Content-Type")
	if !strings.Contains(contentType, "application/xml") {
		t.Errorf("Content-Type = %s, want application/xml", contentType)
	}
	contentDisp := w.Header().Get("Content-Disposition")
	if !strings.Contains(contentDisp, "attachment") || !strings.Contains(contentDisp, "rekordbox.xml") {
		t.Errorf("Content-Disposition = %s", contentDisp)
	}
	bodyStr := w.Body.String()
	if !strings.Contains(bodyStr, `<DJ_PLAYLISTS`) || !strings.Contains(bodyStr, `TrackID="101"`) || !strings.Contains(bodyStr, `Name="Vocal"`) {
		t.Errorf("unexpected xml body: %s", bodyStr)
	}

	// 2. POST /api/rekordbox/export (export to disk)
	postBody := `{"drive":"USB_DRIVE","os":"win","letter":"D"}`
	pReq := httptest.NewRequest(http.MethodPost, "/api/rekordbox/export", strings.NewReader(postBody))
	pReq.Header.Set("Authorization", "Bearer test-token")
	pReq.Header.Set("Content-Type", "application/json")
	pRec := httptest.NewRecorder()
	s.ServeHTTP(pRec, pReq)

	if pRec.Code != http.StatusOK {
		t.Fatalf("POST /api/rekordbox/export code = %d, want 200, body: %s", pRec.Code, pRec.Body.String())
	}
	var resp rekordboxExportResp
	if err := json.Unmarshal(pRec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal post resp: %v", err)
	}
	if !resp.Success || resp.TotalTracks != 1 || resp.CuesCount != 2 { // 1 hot cue + 1 main cue
		t.Errorf("unexpected post resp: %+v", resp)
	}

	// Verify disk file
	xmlFile := filepath.Join(driveDir, "rekordbox.xml")
	data, err := os.ReadFile(xmlFile)
	if err != nil {
		t.Fatalf("read disk xml: %v", err)
	}
	if !strings.Contains(string(data), `file://localhost/D:/Music/vocal.mp3`) {
		t.Errorf("missing D: drive path in exported xml: %s", string(data))
	}

	// 3. Test /v1/enginex/rekordbox/export alias (deck API)
	aliasReq := httptest.NewRequest(http.MethodGet, "/v1/enginex/rekordbox/export?drive=USB_DRIVE", nil)
	aliasReq.RemoteAddr = "127.0.0.1:12345"
	aliasRec := httptest.NewRecorder()
	s.ServeHTTP(aliasRec, aliasReq)
	if aliasRec.Code != http.StatusOK {
		t.Fatalf("GET /v1/enginex/rekordbox/export code = %d, want 200, body: %s", aliasRec.Code, aliasRec.Body.String())
	}

	// 4. Invalid drive error handling
	badReq := httptest.NewRequest(http.MethodGet, "/api/rekordbox/export?drive=../bad", nil)
	badReq.Header.Set("Authorization", "Bearer test-token")
	badRec := httptest.NewRecorder()
	s.ServeHTTP(badRec, badReq)
	if badRec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for bad drive, got %d", badRec.Code)
	}
}
