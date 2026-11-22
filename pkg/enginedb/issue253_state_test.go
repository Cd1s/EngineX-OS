package enginedb

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

// #253: the maintainer's 6000-track stick had a 2.7 GB .rb-bridge-state.db. Each row kept a copy
// of the track's PWV7 (about 100 KB for four minutes, compressed), which is in the track's .2EX
// on the same stick. A row now names the ANLZ files and keeps the source ID.
func TestIssue253StateRowNamesTheANLZFilesInsteadOfCopyingTheWaveform(t *testing.T) {
	root := t.TempDir()
	testfixture.Export(t, root)
	// Four minutes of PWV7 for every track, as a real .2EX has.
	rnd := rand.New(rand.NewSource(253))
	wave := make([]byte, 12+3*36000)
	binary.BigEndian.PutUint32(wave, 3)
	binary.BigEndian.PutUint32(wave[4:], 36000)
	rnd.Read(wave[12:])
	testfixture.Write(t, root, "PIONEER/USBANLZ/P001/ANLZ0000.2EX", testfixture.PMAI(testfixture.Section("PWV7", 24, wave)))
	l, err := rekordbox.ReadPDB(filepath.Join(root, "PIONEER/rekordbox/export.pdb"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Sync(context.Background(), root, l, Options{}); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(root, "Engine Library/Database2", stateFile)
	db, err := sql.Open("sqlite", state)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n, biggest int
	if err = db.QueryRow(`SELECT count(*),max(length(sourceJSON)+length(audioJSON)) FROM BridgeAnalysis`).Scan(&n, &biggest); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(state)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d rows, the largest %d bytes; state file %d bytes", n, biggest, st.Size())
	if n == 0 || biggest > 256 || st.Size() > 64<<10 {
		t.Fatalf("state keeps the waveform: %d rows, the largest %d bytes, file %d bytes", n, biggest, st.Size())
	}
	var row []byte
	if err = db.QueryRow(`SELECT sourceJSON FROM BridgeAnalysis WHERE trackId=1`).Scan(&row); err != nil {
		t.Fatal(err)
	}
	plain, err := UnpackAnalysis(row, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	var got struct{ Track struct{ ID uint32 } }
	if json.Unmarshal(plain, &got) != nil || got.Track.ID != 1 {
		t.Fatalf("source ID lost: %s", plain)
	}
	if id, err := analysisSourceID(row); err != nil || id != 1 {
		t.Fatalf("identity prefix: %d %v", id, err)
	}
}
