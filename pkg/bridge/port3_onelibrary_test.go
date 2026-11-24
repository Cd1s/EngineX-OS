package bridge_test

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	bridge "github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/bridge/bridgetest"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
	_ "modernc.org/sqlite"
)

const p3OneLibraryPassphrase = "r8gddnr4k847830ar6cqzbkk0el6qytmb3trbbx805jm74vez64i5o8fnrqryqls"

func p3EncryptedOneLibrary(t *testing.T, mount string) {
	t.Helper()
	plainSource := filepath.Join("..", "..", "rekordbox", "testdata", "sqlcipher4_onelibrary_decrypted.db")
	encryptedSource := filepath.Join("..", "..", "rekordbox", "testdata", "sqlcipher4_onelibrary.db")
	plainPath := filepath.Join(t.TempDir(), "onelibrary.db")
	raw, err := os.ReadFile(plainSource)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plainPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", plainPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
		PRAGMA journal_mode=DELETE;
		ALTER TABLE content ADD COLUMN analysisDataFilePath varchar;
		ALTER TABLE content ADD COLUMN genre_id INTEGER;
		ALTER TABLE content ADD COLUMN label_id INTEGER;
		CREATE TABLE genre(genre_id INTEGER PRIMARY KEY, name TEXT);
		CREATE TABLE label(label_id INTEGER PRIMARY KEY, name TEXT);
		INSERT INTO genre VALUES (20, 'Old Genre');
		INSERT INTO label VALUES (30, 'Old Label');
		DELETE FROM content;
		DELETE FROM cue;
	`)
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	paths := []string{"/" + testfixture.LargeTrackPath(1), "/" + testfixture.LargeTrackPath(2)}
	_, err = db.Exec(`INSERT INTO content(content_id,title,bpmx100,length,path,samplingRate,djComment,analysisDataFilePath,genre_id,label_id) VALUES
		(1,'Track 1',12800,4,?,44100,'Old Comment','/PIONEER/USBANLZ/P001/ANLZ0000.DAT',20,30),
		(2,'Track 2',12400,4,?,44100,'','/PIONEER/USBANLZ/P001/ANLZ0000.DAT',20,30)`, paths[0], paths[1])
	_ = db.Close()
	if err != nil {
		t.Fatal(err)
	}
	salt, err := rekordbox.ReadSQLCipherSalt(encryptedSource)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := os.Open(plainPath)
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	info, err := plain.Stat()
	if err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(mount, "PIONEER", "rekordbox", "exportLibrary.db")
	out, err := os.Create(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := rekordbox.EncryptSQLCipher4(context.Background(), plain, info.Size(), out, salt, []byte(p3OneLibraryPassphrase)); err != nil {
		_ = out.Close()
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestP3MetadataWriteBackUpdatesPDBAndOneLibrary(t *testing.T) {
	mount := t.TempDir()
	testfixture.LargeExport(t, mount, 2)
	p3EncryptedOneLibrary(t, mount)
	bridgetest.Import(t, mount)
	db := p3OpenDB(t, mount)
	id := p3FirstTrackID(t, db)
	if _, err := db.Exec(`UPDATE Track SET genre=?, comment=?, label=? WHERE id=?`, "Both Genre", "Both Comment", "Both Label", id); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE Track SET lastEditTime=lastEditTime+7 WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	res, err := bridge.WriteBackCues(context.Background(), mount, bridge.WriteBackOptions{DateDir: "2026-11-24", SkipPreparingCheck: true})
	if err != nil || res.TracksChanged != 1 || !res.OneLibraryUpdated {
		t.Fatalf("both-library metadata write-back: %+v %v", res, err)
	}
	pdb, err := rekordbox.ReadPDB(filepath.Join(mount, "PIONEER", "rekordbox", "export.pdb"))
	if err != nil {
		t.Fatal(err)
	}
	if pdb.Tracks[0].Genre != "Both Genre" || pdb.Tracks[0].Comment != "Both Comment" || pdb.Tracks[0].Label != "Both Label" {
		t.Fatalf("PDB metadata: %+v", pdb.Tracks[0])
	}
	keys := rekordbox.NewKeyCache()
	defer keys.Clear()
	one, err := rekordbox.ReadSQLiteKeys(context.Background(), filepath.Join(mount, "PIONEER", "rekordbox", "exportLibrary.db"), nil, keys)
	if err != nil || len(one.Tracks) == 0 {
		t.Fatalf("OneLibrary read: %v", err)
	}
	if one.Tracks[0].Genre != "Both Genre" || one.Tracks[0].Comment != "Both Comment" || one.Tracks[0].Label != "Both Label" {
		t.Fatalf("OneLibrary metadata: %+v", one.Tracks[0])
	}
	if raw, err := os.ReadFile(filepath.Join(mount, "PIONEER", "rekordbox", "exportLibrary.db")); err != nil || bytes.Equal(raw, nil) {
		t.Fatalf("OneLibrary output missing: %v", err)
	}
}
