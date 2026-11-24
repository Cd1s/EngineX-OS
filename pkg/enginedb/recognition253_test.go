package enginedb

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func recognition253Fixture(t testing.TB, n int) (string, *rekordbox.Library, *sql.DB, []preparedTrack) {
	t.Helper()
	root := t.TempDir()
	testfixture.LargeExport(t, root, n)
	l, err := rekordbox.ReadPDB(filepath.Join(root, "PIONEER/rekordbox/export.pdb"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Sync(context.Background(), root, l, Options{Fast: true}); err != nil {
		t.Fatal(err)
	}
	r, err := compat.NewResolver(root)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := prepareFast(context.Background(), r, l, false)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(root, "Engine Library/Database2/m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1)
	if _, err = db.Exec(`ATTACH DATABASE ? AS bridge`, filepath.Join(root, "Engine Library/Database2", stateFile)); err != nil {
		t.Fatal(err)
	}
	return root, l, db, prepared
}

func TestRepeatImportDoesNotReadUnneededAnalysis253(t *testing.T) {
	root, l, db, _ := recognition253Fixture(t, 600)
	// Source ID and path already agree. Analysis payloads are not required to
	// identify these rows; a missing/corrupt detail must not block the import.
	if _, err := db.Exec(`INSERT INTO BridgeAnalysis(trackId,sourceJSON,audioJSON) SELECT trackId,x'1f8b0000','{}' FROM BridgeState`); err != nil {
		t.Fatal(err)
	}
	r, err := Sync(context.Background(), root, l, Options{Fast: true})
	if err != nil || r.Added != 0 || r.Unchanged != 600 || r.PlaylistsChanged {
		t.Fatalf("repeat import: %+v %v", r, err)
	}
}

func TestLargePDBPreparationProgress253(t *testing.T) {
	root := t.TempDir()
	testfixture.LargeExport(t, root, 6000)
	l, err := rekordbox.ReadPDB(filepath.Join(root, "PIONEER/rekordbox/export.pdb"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := compat.NewResolver(root)
	if err != nil {
		t.Fatal(err)
	}
	oldStat, oldHeader := statFile, audioHeader
	t.Cleanup(func() { statFile, audioHeader = oldStat, oldHeader })
	clock := time.Now()
	cutoff := clock.Add(time.Millisecond)
	stats, headers := 0, 0
	statFile = func(p string) (os.FileInfo, error) {
		stats++
		clock = clock.Add(2 * time.Millisecond)
		if p == filepath.Join(root, testfixture.LargeTrackPath(1)) {
			return nil, &os.PathError{Op: "stat", Path: p, Err: os.ErrNotExist}
		}
		return oldStat(p)
	}
	audioHeader = func(p string) (compat.AudioInfo, error) {
		headers++
		clock = clock.Add(2 * time.Millisecond)
		return oldHeader(p)
	}
	start := time.Now()
	prepared, err := prepareTracks(context.Background(), r, l, cutoff, func() time.Time { return clock }, nil)
	if err != nil {
		t.Fatal(err)
	}
	ready := 0
	for _, p := range prepared {
		if !p.late {
			ready++
		}
	}
	if ready != 1 || stats > 5 || headers != 1 {
		t.Fatalf("no bounded progress: ready=%d stats=%d headers=%d", ready, stats, headers)
	}
	t.Logf("6000 tracks: preparation=%s ready=%d deferred=%d stat_calls=%d header_reads=%d", time.Since(start), ready, len(prepared)-ready, stats, headers)
}

func BenchmarkRepeatLargePDBIdentity253(b *testing.B) {
	for _, n := range []int{600, 6000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			_, _, db, prepared := recognition253Fixture(b, n)
			rng := rand.New(rand.NewSource(253))
			data := make([]byte, 250<<10)
			for i := range data {
				data[i] = byte(rng.Intn(128))
			}
			padding, err := json.Marshal(data)
			if err != nil {
				b.Fatal(err)
			}
			tx, err := db.Begin()
			if err != nil {
				b.Fatal(err)
			}
			for id := 1; id <= 600; id++ {
				source := append([]byte(fmt.Sprintf(`{"Track":{"ID":%d},"Padding":`, id)), padding...)
				source = append(source, '}')
				packed, err := packAnalysis(source)
				if err != nil {
					b.Fatal(err)
				}
				if _, err = tx.Exec(`INSERT INTO BridgeAnalysis(trackId,sourceJSON,audioJSON) VALUES(?,?,'{}')`, id, packed); err != nil {
					b.Fatal(err)
				}
			}
			if err = tx.Commit(); err != nil {
				b.Fatal(err)
			}
			tx, err = db.Begin()
			if err != nil {
				b.Fatal(err)
			}
			defer tx.Rollback()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				owned, _, _, err := stageOwnedMoves(context.Background(), tx, prepared, false)
				if err != nil || len(owned) != n {
					b.Fatalf("identity pass: %d %v", len(owned), err)
				}
			}
		})
	}
}

func TestLegacyIdentityDoesNotReadAnalysisPayload(t *testing.T) {
	root, l, db, _ := recognition253Fixture(t, 10)
	for id := 1; id <= 10; id++ {
		source := []byte(fmt.Sprintf(`{"Track":{"ID":%d},"Analysis":{"Padding":%q}}`, id, strings.Repeat("waveform", 64<<10)))
		packed, err := packAnalysis(source)
		if err != nil {
			t.Fatal(err)
		}
		packed[len(packed)-1] ^= 1 // the unused analysis tail cannot validate as a whole
		if _, err = UnpackAnalysis(packed, 64<<20); err == nil {
			t.Fatal("fixture must fail a full payload read")
		}
		if _, err = db.Exec(`INSERT INTO BridgeAnalysis(trackId,sourceJSON,audioJSON) VALUES(?,?,'{}')`, id, packed); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`UPDATE Track SET pdbImportKey=67`); err != nil {
		t.Fatal(err)
	}
	l.Tracks[0].Path, l.Tracks[1].Path = l.Tracks[1].Path, l.Tracks[0].Path
	r, err := Sync(context.Background(), root, l, Options{Fast: true})
	if err != nil || r.Added != 0 {
		t.Fatalf("legacy identity read the analysis payload: %+v %v", r, err)
	}
	for id, want := range map[int]string{1: "../" + testfixture.LargeTrackPath(2), 2: "../" + testfixture.LargeTrackPath(1)} {
		var got string
		if err = db.QueryRow(`SELECT path FROM Track WHERE id=?`, id).Scan(&got); err != nil || got != want {
			t.Fatalf("track %d: path=%q %v", id, got, err)
		}
	}
}

func BenchmarkLegacySharedKeyIdentity253(b *testing.B) {
	for _, format := range []string{"plain", "gzip"} {
		b.Run(format, func(b *testing.B) {
			_, _, db, prepared := recognition253Fixture(b, 6000)
			rng := rand.New(rand.NewSource(253))
			data := make([]byte, 250<<10)
			for i := range data {
				data[i] = byte(rng.Intn(128))
			}
			padding, err := json.Marshal(data)
			if err != nil {
				b.Fatal(err)
			}
			tx, err := db.Begin()
			if err != nil {
				b.Fatal(err)
			}
			for id := 1; id <= 600; id++ {
				source := append([]byte(fmt.Sprintf(`{"Track":{"ID":%d},"Padding":`, id)), padding...)
				source = append(source, '}')
				if format == "gzip" {
					source, err = packAnalysis(source)
					if err != nil {
						b.Fatal(err)
					}
				}
				if _, err = tx.Exec(`INSERT INTO BridgeAnalysis(trackId,sourceJSON,audioJSON) VALUES(?,?,'{}')`, id, source); err != nil {
					b.Fatal(err)
				}
			}
			if _, err = tx.Exec(`UPDATE Track SET pdbImportKey=67`); err != nil {
				b.Fatal(err)
			}
			if err = tx.Commit(); err != nil {
				b.Fatal(err)
			}
			tx, err = db.Begin()
			if err != nil {
				b.Fatal(err)
			}
			defer tx.Rollback()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				owned, _, _, err := stageOwnedMoves(context.Background(), tx, prepared, false)
				if err != nil || len(owned) != 6000 {
					b.Fatalf("legacy identity pass: %d %v", len(owned), err)
				}
			}
		})
	}
}
