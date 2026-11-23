package bridge_test

import (
	"context"
	"database/sql"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

// #321: a write-back run derives the exportLibrary.db keys (PBKDF2, 256 000 rounds) once for
// its read, decrypt and encrypt; a long-lived caller that keeps them derives none on its next run.
func TestIssue321OneKeyDerivationPerRun(t *testing.T) {
	if bridge.InChildForTest(t) {
		return
	}
	ctx := context.Background()
	stick := setupWriteBackStick(t, true)
	if _, err := convertWriteBackAtTest(ctx, stick); err != nil {
		t.Fatalf("Convert: %v", err)
	}
	injectDeckCueEdit(t, stick)
	d0 := rekordbox.KeyDerivations()
	res, err := bridge.WriteBackCues(ctx, stick, bridge.WriteBackOptions{DateDir: "2026-11-22", SkipPreparingCheck: true})
	if err != nil || !res.OneLibraryUpdated {
		t.Fatalf("WriteBackCues: %+v %v", res, err)
	}
	if n := rekordbox.KeyDerivations() - d0; n != 1 {
		t.Fatalf("one run derived the keys %d times, want 1", n)
	}
	lib, err := rekordbox.ReadSQLiteKeys(ctx, filepath.Join(stick, "PIONEER", "rekordbox", "exportLibrary.db"), nil, testKeys)
	if err != nil || len(lib.Tracks) != 2 {
		t.Fatalf("exportLibrary.db after the run: %v", err)
	}

	keys := rekordbox.NewKeyCache()
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: filepath.Join(stick, "Engine Library", "Database2", "m.db")}).String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for i, name := range []string{"Kept key 1", "Kept key 2"} {
		for s := time.Now().Unix(); time.Now().Unix() == s; {
			time.Sleep(20 * time.Millisecond)
		}
		issue321Edit(t, db, 2, name, uint32(1500+500*i))
		d0 = rekordbox.KeyDerivations()
		res, err = bridge.WriteBackCues(ctx, stick, bridge.WriteBackOptions{DateDir: "2026-11-22", SkipPreparingCheck: true, Keys: keys})
		if err != nil || !res.OneLibraryUpdated || !issue321ANLZHas(t, stick, name) {
			t.Fatalf("run %d with kept keys: %+v %v", i+1, res, err)
		}
		if n, want := rekordbox.KeyDerivations()-d0, int64(1-i); n != want {
			t.Fatalf("run %d with kept keys derived %d times, want %d", i+1, n, want)
		}
	}
	keys.Clear()
}
