package enginedb

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strconv"

	"github.com/Cd1s/sc5000-custom-os/pkg/compat"
	"github.com/Cd1s/sc5000-custom-os/pkg/rekordbox"
	"github.com/google/uuid"
)

type preparedTrack struct {
	track     rekordbox.Track
	rel, hash string
	info      os.FileInfo
	files     []analysisFile
	art       *analysisFileExport
}

func prepareTracks(ctx context.Context, res *compat.Resolver, l *rekordbox.Library) ([]preparedTrack, error) {
	prepared := make([]preparedTrack, 0, len(l.Tracks))
	paths := map[string]uint32{}
	for _, t := range l.Tracks {
		if e := ctx.Err(); e != nil {
			return nil, e
		}
		rel, e := res.Resolve(t.Path)
		if e != nil {
			return nil, fmt.Errorf("track %d: %w", t.ID, e)
		}
		if other, ok := paths[rel]; ok {
			return nil, fmt.Errorf("tracks %d and %d resolve to the same audio path %q", other, t.ID, rel)
		}
		paths[rel] = t.ID
		st, e := os.Stat(res.FullPath(rel))
		if e != nil {
			return nil, e
		}
		if !st.Mode().IsRegular() {
			return nil, fmt.Errorf("audio not regular: %s", rel)
		}
		files, art, e := sourceFiles(res, t)
		if e != nil {
			return nil, fmt.Errorf("track %d: %w", t.ID, e)
		}
		fp := fingerprint{Record: t.RecordHash + ":" + rekordbox.RecordHash(t), ResolvedPath: rel, Size: st.Size(), MTime: st.ModTime().UnixNano(), Artwork: art}
		for _, f := range files {
			fp.Analysis = append(fp.Analysis, analysisFileExport{f.rel, f.size, f.mtime})
		}
		prepared = append(prepared, preparedTrack{t, rel, digest(fp), st, files, art})
	}
	return prepared, nil
}

// Stage every moved owned row first, so even two source tracks swapping paths
// can retain their Engine IDs without violating Track's immediate UNIQUE(path).
// Both temporary paths and the final rename are in the import transaction.
func stageOwnedMoves(ctx context.Context, tx *sql.Tx, prepared []preparedTrack) (map[uint32]int64, error) {
	targets := map[uint32]string{}
	for _, p := range prepared {
		targets[p.track.ID] = "../" + p.rel
	}
	rows, e := tx.QueryContext(ctx, `SELECT s.trackId,t.pdbImportKey,s.path FROM BridgeState s JOIN Track t ON t.id=s.trackId`)
	if e != nil {
		return nil, e
	}
	type move struct {
		id     int64
		source uint32
	}
	var moves []move
	owned := map[uint32]int64{}
	for rows.Next() {
		var id int64
		var source uint32
		var oldPath string
		if e = rows.Scan(&id, &source, &oldPath); e != nil {
			rows.Close()
			return nil, e
		}
		target, ok := targets[source]
		if !ok {
			continue
		}
		if _, duplicate := owned[source]; duplicate {
			rows.Close()
			return nil, fmt.Errorf("ambiguous bridge source ID %d", source)
		}
		owned[source] = id
		if target != oldPath {
			moves = append(moves, move{id, source})
		}
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, e
	}
	prefix := ".rb-bridge-pending-" + uuid.NewString() + "/"
	for _, m := range moves {
		temporary := prefix + strconv.FormatInt(m.id, 10)
		if _, e = tx.ExecContext(ctx, `UPDATE Track SET path=? WHERE id=?`, temporary, m.id); e != nil {
			return nil, e
		}
		if _, e = tx.ExecContext(ctx, `UPDATE BridgeState SET path=?,fingerprint='' WHERE trackId=?`, temporary, m.id); e != nil {
			return nil, e
		}
	}
	return owned, nil
}
