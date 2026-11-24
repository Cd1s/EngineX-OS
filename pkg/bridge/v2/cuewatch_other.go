//go:build !linux

package bridge

import "context"

// Run needs Linux (inotify, mountinfo): elsewhere only Final and Pass are served.
func (w *CueWatcher) Run(ctx context.Context) error {
	<-ctx.Done()
	return nil
}
