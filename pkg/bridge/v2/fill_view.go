package bridge

import (
	"context"
	"errors"
	"os"

	oldbridge "github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

// readFillView reads the stick's exports into the tracks the job works on: each track of the
// view once, by key, with the hash a window compares Sig.Row with, and the view's stamp.
//
// LOCAL ADAPTER (fill lane, until FindSources, ReadSources and BuildView of design §1.4 are on
// the branch): it reads the exports with the old pkg/bridge reader and hashes the export fields
// itself. At integration its body becomes
//
//	files, err := FindSources(r, mount)
//	libs, _, err := ReadSources(ctx, r, mount, files, nil)
//	v := BuildView(libs, layout, "")
//	// fillView{stamp: v.Stamp, tracks: {Key, Track, Hash} of each v.Tracks}
//
// and the import of the old package goes.
var readFillView = func(ctx context.Context, r *compat.Resolver, mount, layout string) (*fillView, error) {
	l, err := oldbridge.ReadLibrary(ctx, mount)
	if err != nil {
		return nil, err
	}
	v := &fillView{tracks: make([]fillTrack, 0, len(l.Tracks))}
	seen := make(map[string]bool, len(l.Tracks))
	for _, t := range l.Tracks {
		rel, err := compat.CleanRelative(t.Path)
		if err != nil {
			continue
		}
		key := rbstate.FoldPath(rel)
		if seen[key] {
			continue
		}
		seen[key] = true
		v.tracks = append(v.tracks, fillTrack{key: key, track: t, hash: t.RecordHash + ":" + rekordbox.RecordHash(t)})
	}
	var files []rbstate.FileSig
	for _, name := range []string{"PIONEER/rekordbox/export.pdb", "PIONEER/rekordbox/exportLibrary.db"} {
		rel, err := r.ResolveContext(ctx, name)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		st, err := os.Stat(r.FullPath(rel))
		if err != nil {
			return nil, err
		}
		files = append(files, rbstate.FileSig{Rel: rel, Size: st.Size(), MTimeNS: st.ModTime().UnixNano()})
	}
	v.stamp = rbstate.Stamp(layout, files, "")
	return v, nil
}
