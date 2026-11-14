package bridge_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

func TestLegacyImportPreservesEngineRowsAndEdits(t *testing.T) {
	for _, damagedKey := range []bool{true, false} {
		t.Run(fmt.Sprintf("shared-import-key=%v", damagedKey), func(t *testing.T) {
			root := t.TempDir()
			testfixture.LargeExport(t, root, 10)
			ctx := context.Background()
			if _, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil {
				t.Fatal(err)
			}
			db := dbOpen(t, root)
			listID := scalar[int64](t, db, `SELECT id FROM Playlist WHERE title='Large Set'`)
			members := entityChain(t, db, listID)
			if _, err := db.Exec(`UPDATE Track SET dateAdded=1738108800,isPlayed=CASE WHEN id BETWEEN 3 AND 8 THEN 1 ELSE 0 END`); err != nil {
				t.Fatal(err)
			}
			// The real old library had the same import key on all ten rows, but
			// BridgeAnalysis retained the correct source IDs and BridgeState paths.
			if damagedKey {
				if _, err := db.Exec(`UPDATE Track SET pdbImportKey=67`); err != nil {
					t.Fatal(err)
				}
			}
			cues, loops := map[int][]byte{}, map[int][]byte{}
			adjusted := map[int]float64{3: 166990, 5: 1201632, 7: 8137920, 8: 13773577}
			for id := 3; id <= 10; id++ {
				p, err := enginedb.EncodePerformance(rekordbox.Track{SampleRate: 44100}, &rekordbox.Analysis{Cues: []rekordbox.Cue{
					{HotCue: 1, TimeMS: 1000, Name: "User hot cue"},
					{HotCue: 2, TimeMS: 2000, EndMS: 3000, Loop: true, Active: true, Name: "User loop"},
				}})
				if err != nil {
					t.Fatal(err)
				}
				raw, err := enginedb.QUncompress(p.QuickCues, 64<<10)
				if err != nil {
					t.Fatal(err)
				}
				if pos, ok := adjusted[id]; ok {
					binary.BigEndian.PutUint64(raw[len(raw)-17:], math.Float64bits(pos))
					raw[len(raw)-9] = 1 // Engine's user-adjusted main cue flag.
				}
				cues[id], err = enginedb.QCompress(raw)
				if err != nil {
					t.Fatal(err)
				}
				loops[id] = p.Loops
				if _, err := db.Exec(`UPDATE PerformanceData SET thirdPartySourceId=NULL,quickCues=?,loops=?,activeOnLoadLoops=? WHERE trackId=?`, cues[id], loops[id], p.ActiveLoops, id); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := db.Exec(`UPDATE BridgeState SET fingerprint='legacy'; UPDATE BridgeLibrary SET value='legacy-playlist-hash' WHERE key='playlists'`); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(root, "Engine Library", "Database2")
			if err := os.WriteFile(filepath.Join(dir, ".rb-bridge-stamp"), []byte("PIONEER/rekordbox/export.pdb 208896 1738170356000000000\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			r, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{Deadline: time.Now().Add(time.Hour)}, false)
			if err != nil {
				t.Fatal(err)
			}
			if r.Added != 0 || r.Deferred != 0 {
				t.Errorf("existing tracks recreated or unfinished: %+v", r)
			}
			if id := scalar[int64](t, db, `SELECT id FROM Playlist WHERE title='Large Set'`); id != listID {
				t.Errorf("playlist ID changed: %d -> %d", listID, id)
			}
			if got := entityChain(t, db, listID); !reflect.DeepEqual(got, members) {
				t.Errorf("playlist membership/order changed: %v -> %v", members, got)
			}
			for id := 1; id <= 10; id++ {
				var played, added int64
				var quick, loop []byte
				var active, source, key int
				var path string
				if err := db.QueryRow(`SELECT COALESCE(isPlayed,0),dateAdded,quickCues,loops,COALESCE(activeOnLoadLoops,0),COALESCE(thirdPartySourceId,-1),pdbImportKey,path FROM Track JOIN PerformanceData ON trackId=id WHERE id=?`, id).Scan(&played, &added, &quick, &loop, &active, &source, &key, &path); err != nil {
					t.Errorf("lost track %d: %v", id, err)
					continue
				}
				wantPlayed := int64(0)
				if id >= 3 && id <= 8 {
					wantPlayed = 1
				}
				if played != wantPlayed || added != 1738108800 {
					t.Errorf("track %d: played=%d added=%d", id, played, added)
				}
				if key != id || path != "../"+testfixture.LargeTrackPath(id) {
					t.Errorf("track %d: wrong source/path mapping: key=%d path=%q", id, key, path)
				}
				if id >= 3 && (!bytes.Equal(quick, cues[id]) || !bytes.Equal(loop, loops[id]) || active != 2 || source != -1) {
					t.Errorf("track %d: Engine cues/loops were overwritten", id)
				}
			}
			if r, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil || !r.Bypassed || r.PlaylistsChanged {
				t.Fatalf("completed import is not stable: %+v %v", r, err)
			}
		})
	}
}

func TestLegacyImportMissingSourceRecordsUsesOwnedPaths(t *testing.T) {
	for _, state := range []string{"missing", "malformed"} {
		t.Run(state, func(t *testing.T) {
			root := t.TempDir()
			testfixture.LargeExport(t, root, 10)
			ctx := context.Background()
			if _, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false); err != nil {
				t.Fatal(err)
			}
			db := dbOpen(t, root)
			query := `DELETE FROM BridgeAnalysis`
			if state == "malformed" {
				query = `UPDATE BridgeAnalysis SET sourceJSON=x''`
			}
			if _, err := db.Exec(query + `; UPDATE Track SET pdbImportKey=67; UPDATE BridgeState SET fingerprint='legacy'`); err != nil {
				t.Fatal(err)
			}
			if _, err := enginedb.Sync(ctx, root, &rekordbox.Library{}, enginedb.Options{StagedOnly: true}); err != nil {
				t.Fatalf("remote-only import tried to identify absent local sources: %v", err)
			}
			if n := scalar[int](t, db, `SELECT count(*) FROM Track`); n != 10 {
				t.Fatalf("remote-only import removed local tracks: %d", n)
			}
			l, err := bridge.ReadLibrary(ctx, root)
			if err != nil {
				t.Fatal(err)
			}
			l.Tracks[0].Path = "/Contents/unknown.mp3"
			testfixture.Write(t, root, "Contents/unknown.mp3", testfixture.MP3WithLAMETag())
			if _, err := enginedb.Sync(ctx, root, l, enginedb.Options{}); err == nil {
				t.Fatal("accepted an ambiguous shared key with no matching owned path")
			}
			if n := scalar[int](t, db, `SELECT count(*) FROM Track WHERE pdbImportKey=67 AND path NOT LIKE '.rb-bridge-pending-%'`); n != 10 {
				t.Fatalf("ambiguous import changed old rows instead of rolling back: %d", n)
			}
			dir := filepath.Join(root, "Engine Library", "Database2")
			if err := os.WriteFile(filepath.Join(dir, ".rb-bridge-stamp"), []byte("PIONEER/rekordbox/export.pdb old\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			r, err := bridge.ConvertAtMount(ctx, root, enginedb.Options{}, false)
			if err != nil || r.Added != 0 || r.Updated != 10 {
				t.Fatalf("shared keys with %s source records: %+v %v", state, r, err)
			}
			for id := 1; id <= 10; id++ {
				if path := scalar[string](t, db, `SELECT path FROM Track WHERE id=? AND pdbImportKey=?`, id, id); path != "../"+testfixture.LargeTrackPath(id) {
					t.Fatalf("lost identity of track %d: %q", id, path)
				}
			}
		})
	}
}
