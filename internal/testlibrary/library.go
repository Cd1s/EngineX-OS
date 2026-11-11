// Package testlibrary creates fixtures using the exact captured Engine DDL.
package testlibrary

import (
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func schemaSQL(t testing.TB) string {
	t.Helper()
	_, source, _, _ := runtime.Caller(0)
	b, err := os.ReadFile(filepath.Join(filepath.Dir(source), "../../docs/re-report/ENGINE-5.0.4-m.db-schema.sql"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(string(b), "CREATE TABLE sqlite_sequence(name,seq);", "")
}

func Create(t *testing.T) (path, root string, db *sql.DB) {
	t.Helper()
	root = t.TempDir()
	path = filepath.Join(root, "Engine Library", "Database2", "m.db")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	var err error
	db, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err = db.Exec(schemaSQL(t)); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("INSERT INTO Information(uuid,schemaVersionMajor,schemaVersionMinor,schemaVersionPatch) VALUES('test-library-uuid',3,0,2)"); err != nil {
		t.Fatal(err)
	}
	return
}

func writeWAV(t testing.TB, path string, freqHz float64) int64 {
	t.Helper()
	const sampleRate = 44100
	const samples = sampleRate / 4 // 250 ms mono 16-bit PCM
	dataBytes := samples * 2
	buf := make([]byte, 44+dataBytes)
	copy(buf[0:4], "RIFF")
	binary.LittleEndian.PutUint32(buf[4:8], uint32(36+dataBytes))
	copy(buf[8:12], "WAVE")
	copy(buf[12:16], "fmt ")
	binary.LittleEndian.PutUint32(buf[16:20], 16)
	binary.LittleEndian.PutUint16(buf[20:22], 1) // PCM
	binary.LittleEndian.PutUint16(buf[22:24], 1) // mono
	binary.LittleEndian.PutUint32(buf[24:28], sampleRate)
	binary.LittleEndian.PutUint32(buf[28:32], sampleRate*2)
	binary.LittleEndian.PutUint16(buf[32:34], 2)
	binary.LittleEndian.PutUint16(buf[34:36], 16)
	copy(buf[36:40], "data")
	binary.LittleEndian.PutUint32(buf[40:44], uint32(dataBytes))
	for i := 0; i < samples; i++ {
		s := int16(8000 * math.Sin(2*math.Pi*freqHz*float64(i)/sampleRate))
		binary.LittleEndian.PutUint16(buf[44+i*2:], uint16(s))
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf, 0644); err != nil {
		t.Fatal(err)
	}
	return int64(len(buf))
}

// ExportDrive populates root as a mounted internal/USB drive containing an
// Engine Library (Engine Library/Database2/m.db, schema 3.0.2) and playable WAV
// tracks under Music/.
func ExportDrive(t testing.TB, root string) {
	t.Helper()
	const uuid = "gate-internal-library-uuid"
	const trackCount = 8
	dbPath := filepath.Join(root, "Engine Library", "Database2", "m.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.Exec("PRAGMA journal_mode=DELETE"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(schemaSQL(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO Information(uuid,schemaVersionMajor,schemaVersionMinor,schemaVersionPatch) VALUES(?,3,0,2)", uuid); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO Playlist(title,parentListId,isPersisted,nextListId,lastEditTime,isExplicitlyExported) VALUES('Gate Internal Set',0,1,0,1700000000,1)"); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= trackCount; i++ {
		fname := fmt.Sprintf("Track%03d.wav", i)
		size := writeWAV(t, filepath.Join(root, "Music", fname), 220+float64(i*20))
		rel := filepath.ToSlash(filepath.Join("..", "..", "Music", fname))
		bpm := 120 + i
		if _, err := db.Exec(`INSERT INTO Track(
			playOrder,length,bpm,year,path,filename,bitrate,bpmAnalyzed,fileBytes,
			title,artist,album,genre,key,rating,isPlayed,fileType,isAnalyzed,
			dateCreated,dateAdded,isAvailable,isMetadataOfPackedTrackChanged,
			isPerfomanceDataOfPackedTrackChanged,playedIndicator,isMetadataImported
		) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,0,0,'wav',1,1700000000,1700000000,1,0,0,0,1)`,
			i, 180+i, bpm, 2026, rel, fname, 705, float64(bpm), size,
			fmt.Sprintf("Gate Track %03d", i), "EngineX Gate", "Internal Drive", "House", i%12,
		); err != nil {
			t.Fatal(err)
		}
		nextID := i + 1
		if i == trackCount {
			nextID = 0
		}
		if _, err := db.Exec("INSERT INTO PlaylistEntity(listId,trackId,databaseUuid,nextEntityId,membershipReference) VALUES(1,?,?,?,0)",
			i, uuid, nextID); err != nil {
			t.Fatal(err)
		}
	}
}
