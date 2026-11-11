package webui

import (
	"net/http"
	"strconv"
	"time"
)

// controllerSettings is all the controller engine's GlobalAssignmentConfig.qml takes
// from the saved settings: pad LED brightness (#85), Swap Loop / Beat Jump (#135) and
// Pitch Fader Pickup (#181). Seen is their digest, sent back as ?seen= on the next request.
type controllerSettings struct {
	LedBrightness int    `json:"ledBrightness"`
	SwapLoopOrder bool   `json:"swapLoopOrder"`
	PitchPickup   string `json:"pitchPickup"`
	Seen          string `json:"seen"`
}

// controllerHold is how long an unchanged answer is held: under the server's
// two-minute WriteTimeout, and about one request a minute while nothing changes.
var controllerHold = 55 * time.Second

// effectivePitchPickup is the mode the deck runs: CDJ only where the board is on
// the pitchPickupBoards list, whatever settings.conf says.
func effectivePitchPickup(saved, board string) string {
	if saved == "cdj" && pitchPickupBoards[board] {
		return "cdj"
	}
	return "stock"
}

// uiChanged wakes the held controller requests. Caller holds uiMu.
func (s *Server) uiChanged() {
	if s.uiNotify != nil {
		close(s.uiNotify)
		s.uiNotify = make(chan struct{})
	}
}

// controllerStatus answers GET /v1/sc5000/controller?seen=DIGEST. With another digest
// (or none) it answers at once. With the current one it holds the request until a
// setting is saved or controllerHold passes, so the controller engine learns of a change
// when it happens without asking again and again.
func (s *Server) controllerStatus(w http.ResponseWriter, r *http.Request) {
	seen := r.URL.Query().Get("seen")
	deadline := time.NewTimer(controllerHold)
	defer deadline.Stop()
	board := imagePrefix(deviceCompatible())
	for {
		s.uiMu.Lock()
		v, wake := s.loadUI(), s.uiNotify
		s.uiMu.Unlock()
		c := controllerSettings{LedBrightness: v.LedBrightness, SwapLoopOrder: v.SwapLoopOrder, PitchPickup: effectivePitchPickup(v.PitchPickup, board)}
		c.Seen = strconv.FormatUint(fnvSum(strconv.Itoa(c.LedBrightness)+"|"+strconv.FormatBool(c.SwapLoopOrder)+"|"+c.PitchPickup), 16)
		if c.Seen != seen {
			respond(w, c)
			return
		}
		select {
		case <-wake:
		case <-deadline.C:
			respond(w, c)
			return
		case <-r.Context().Done():
			return
		case <-s.closed:
			return
		}
	}
}
