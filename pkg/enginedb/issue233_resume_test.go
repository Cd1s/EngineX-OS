package enginedb

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/testfixture"
	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

// The preparation cut-off can pass while source files are being stat'ed,
// before the full read deadline. Both uninspected branches must stay pending.
func TestIssue233UninspectedExistingOffsetStaysPending(t *testing.T) {
	for _, legacy := range []bool{true, false} {
		for _, sameHash := range []bool{true, false} {
			name := "changed-record"
			if sameHash {
				name = "legacy-hash-without-MP3Offset"
			}
			if !legacy {
				name += "-completed-v2"
			}
			t.Run(name, func(t *testing.T) {
				root := t.TempDir()
				testfixture.LargeExport(t, root, 32)
				l, err := rekordbox.ReadPDB(filepath.Join(root, "PIONEER/rekordbox/export.pdb"))
				if err != nil {
					t.Fatal(err)
				}
				ctx := context.Background()
				if _, err := Sync(ctx, root, l, Options{}); err != nil {
					t.Fatal(err)
				}
				dir := filepath.Join(root, "Engine Library/Database2")
				db, err := sql.Open("sqlite", filepath.Join(dir, stateFile))
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				if _, err := db.Exec(`UPDATE BridgeState SET fingerprint='legacy'`); err != nil {
					t.Fatal(err)
				}
				if sameHash {
					res, err := compat.NewResolver(root)
					if err != nil {
						t.Fatal(err)
					}
					p, err := prepareOne(res, l.Tracks[1], strings.TrimPrefix(l.Tracks[1].Path, "/"), time.Now().Add(-time.Second), time.Now, true)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := db.Exec(`UPDATE BridgeState SET fingerprint=? WHERE trackId=2`, p.hash); err != nil {
						t.Fatal(err)
					}
				}
				start := time.Now()
				calls := 0
				r, err := Sync(ctx, root, l, Options{LegacyOffsets: legacy, Deadline: start.Add(time.Minute), Now: func() time.Time {
					calls++
					if calls > 3 {
						return start.Add(40 * time.Second)
					}
					return start
				}})
				pending := 0
				if legacy {
					pending = 1
				}
				if err != nil || r.Deferred != pending || r.Updated != 31 {
					t.Fatalf("partial preparation: %+v %v", r, err)
				}
				var fast int
				if err := db.QueryRow(`SELECT count(*) FROM BridgeState WHERE fingerprint LIKE 'fast:%'`).Scan(&fast); err != nil {
					t.Fatal(err)
				}
				if fast != pending {
					t.Fatalf("legacy=%v: %d fast rows, want %d", legacy, fast, pending)
				}
				r, err = Sync(ctx, root, l, Options{Resume: true, LegacyOffsets: legacy})
				if err != nil || r.Updated != pending || r.Deferred != 0 {
					t.Fatalf("forgot uninspected offset: %+v %v", r, err)
				}
			})
		}
	}
}
