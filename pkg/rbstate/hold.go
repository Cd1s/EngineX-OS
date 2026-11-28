package rbstate

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// The rule (#336 Go review R13): a program changes a volume's library only while it holds the
// volume's lock. The library is every database of Engine Library/Database2 with its sidecars,
// and the bridge's state beside them (the snapshot, the next library and its swap, the browse
// index, the queue of pending edits). Reading takes no lock.
//
// The lock travels in the context: who holds it hands it down, and a writer called without it
// takes it itself. One process never waits for itself, and a writer nobody wrapped still locks.

type volumeHold struct {
	id       string
	released atomic.Bool
	parent   *volumeHold
}

type volumeHoldKey struct{}

func heldIn(ctx context.Context, id string) bool {
	h, _ := ctx.Value(volumeHoldKey{}).(*volumeHold)
	for ; h != nil; h = h.parent {
		if h.id == id && !h.released.Load() {
			return true
		}
	}
	return false
}

// HoldVolume returns ctx holding the lock of the volume mounted at mount. A ctx that holds it
// already is returned as it is, with a release that does nothing; otherwise the lock is taken as
// LockVolume takes it, waiting at most wait (ErrLocked after that), and release gives it back.
func HoldVolume(ctx context.Context, mount string, wait time.Duration) (held context.Context, release func(), err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	id, err := volumeID(mount)
	if err != nil {
		return ctx, nil, err
	}
	if heldIn(ctx, id) {
		return ctx, func() {}, nil
	}
	unlock, err := lockVolumeID(mount, id, wait)
	if err != nil {
		return ctx, nil, err
	}
	h := &volumeHold{id: id}
	h.parent, _ = ctx.Value(volumeHoldKey{}).(*volumeHold)
	var once sync.Once
	return context.WithValue(ctx, volumeHoldKey{}, h), func() {
		once.Do(func() {
			h.released.Store(true)
			unlock()
		})
	}, nil
}

// VolumeHeld reports whether ctx holds the lock of the volume mounted at mount.
func VolumeHeld(ctx context.Context, mount string) bool {
	if ctx == nil {
		return false
	}
	id, err := volumeID(mount)
	return err == nil && heldIn(ctx, id)
}
