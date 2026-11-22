package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/bridge"
)

// The Eject's cue write-back (#321): the edisksd hook runs rb-bridge --writeback-cues --on-eject
// and waits 12 s at most. stream-sync's watcher does the pass with the exportLibrary.db keys it
// kept (deriving them costs seconds on a deck) and what it already read; rb-bridge does it
// itself only when stream-sync does not answer (not running, or an earlier build).

// stream-sync's deck-local gateway (loopback only), where it finds a drive by name, and how
// long rb-bridge waits for it: inside the hook's 12 s, with room to exit.
var (
	writeBackGateway = "http://127.0.0.1:8080"
	writeBackMedia   = "/media"
	writeBackWait    = 10500 * time.Millisecond
)

// finalWriteBack asks stream-sync for the Eject's pass of mount. ok is false when stream-sync
// did not take it, and rb-bridge does the pass itself.
func finalWriteBack(ctx context.Context, mount string) (r bridge.WriteBackResult, err error, ok bool) {
	abs, err := filepath.Abs(mount)
	if err != nil || filepath.Dir(abs) != filepath.Clean(writeBackMedia) {
		return r, nil, false
	}
	ctx, cancel := context.WithTimeout(ctx, writeBackWait)
	defer cancel()
	u := writeBackGateway + "/v1/enginex/rekordbox/writeback?final=1&drive=" + url.QueryEscape(filepath.Base(abs))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, nil)
	if err != nil {
		return r, nil, false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			// stream-sync took it and has not finished: no time is left for a pass of our own.
			return r, fmt.Errorf("cue write-back: stream-sync did not finish within %s", writeBackWait), true
		}
		return r, nil, false
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusOK:
		if err = json.Unmarshal(body, &r); err != nil {
			return r, fmt.Errorf("cue write-back: stream-sync answered %q: %w", body, err), true
		}
		return r, nil, true
	case resp.StatusCode == http.StatusBadRequest:
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &e) == nil && e.Error != "" {
			return r, errors.New(e.Error), true
		}
	}
	return r, nil, false // an earlier stream-sync, or none behind the port
}
