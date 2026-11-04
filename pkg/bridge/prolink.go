package bridge

import (
	"context"

	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/prolink"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
)

// MountedSource reads a locally mounted, read-only LAN export. The caller owns
// mounting/authentication; discovery packets are never treated as trusted paths.
type MountedSource struct{ MountPath string }

func (s MountedSource) Library(ctx context.Context, _ prolink.Device) (*rekordbox.Library, error) {
	return ReadLibrary(ctx, s.MountPath)
}

// EngineSink implements the LAN synchronization interface with the same source
// protection, validation, and incremental transaction used by USB imports.
type EngineSink struct {
	MountPath string
	Options   enginedb.Options
}

func (s EngineSink) Import(ctx context.Context, _ prolink.Device, l *rekordbox.Library) error {
	_, e := enginedb.Sync(ctx, s.MountPath, l, s.Options)
	return e
}
