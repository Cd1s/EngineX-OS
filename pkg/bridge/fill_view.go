package bridge

import (
	"context"

	"github.com/Cd1s/EngineX-OS/pkg/compat"
	"github.com/Cd1s/EngineX-OS/pkg/rbstate"
)

// readFillView reads the stick's exports into the tracks the job works on: each track of the
// view once, by key, with the row hash a window compares Sig.Row with, and the stamp of the
// export files as the mount names them: rbstate.Stamp of the snapshot's layout, FindSources and
// the staged CDJ libraries. A library that cannot be read is left out (ReadSources); with none
// readable the job stops with the error.
var readFillView = func(ctx context.Context, r *compat.Resolver, mount, layout string) (*fillView, error) {
	files, err := FindSources(r, mount)
	if err != nil {
		return nil, err
	}
	libs, _, err := ReadSources(ctx, r, mount, files, nil)
	if err != nil {
		return nil, err
	}
	libs = append(libs, readStaged(mount)...)
	v, err := BuildView(libs, layout, "")
	if err != nil {
		return nil, err
	}
	out := &fillView{stamp: rbstate.Stamp(layout, files, stagedStamp(mount)), tracks: make([]fillTrack, len(v.Tracks))}
	for i, t := range v.Tracks {
		out.tracks[i] = fillTrack{key: t.Key, track: t.Track, hash: t.Hash}
	}
	return out, nil
}
