package webui

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
)

// Bluetooth audio out. Engine's own A2DP source sends one output pair of its master bus to the speaker connected in
// the deck's Bluetooth list; engine_ui_hook.c reads which pair from bt_out= in settings.conf (pair n is outputs 2n
// and 2n+1, so the index in this list is the pair). Only layouts measured on a deck are named; any other model
// offers Master, its first pair, until it has been measured.
func bluetoothLayers(board string) []string {
	switch {
	case playerBoards[board]:
		return []string{"Layer A", "Layer B"} // SC5000 / SC6000 series: the player's two decks
	case board == "PRIME4":
		return []string{"Master", "Booth", "Headphones", "Zone"}
	case board == "PRIME2", board == "MIXSTREAMPRO":
		return []string{"Master", "Headphones"}
	}
	return []string{"Master"}
}

type bluetoothStatus struct {
	AssignLayer string   `json:"assignLayer"`
	Layers      []string `json:"layers"`
}

// handleBluetooth reports what the deck sends to a Bluetooth speaker and this model's choices (GET), and picks one
// with assignLayer=<name> in the query or a JSON body (POST).
func (s *Server) handleBluetooth(w http.ResponseWriter, r *http.Request) {
	layers := bluetoothLayers(imagePrefix(deviceCompatible()))
	var req struct {
		AssignLayer *string `json:"assignLayer"`
	}
	switch r.Method {
	case http.MethodGet:
	case http.MethodPost:
		if a := r.URL.Query().Get("assignLayer"); a != "" {
			req.AssignLayer = &a
		}
		d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		d.DisallowUnknownFields()
		if err := d.Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			fail(w, http.StatusBadRequest, errors.New("invalid JSON request"))
			return
		}
	default:
		fail(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	s.uiMu.Lock()
	v := s.loadUI()
	if req.AssignLayer != nil {
		i := slices.Index(layers, strings.TrimSpace(*req.AssignLayer))
		if i < 0 {
			s.uiMu.Unlock()
			fail(w, http.StatusBadRequest, errors.New("invalid assignLayer: must be "+strings.Join(layers, ", ")))
			return
		}
		v.BtOut = i
		if err := s.saveUI(v); err != nil {
			s.uiMu.Unlock()
			fail(w, http.StatusInternalServerError, err)
			return
		}
	}
	s.uiMu.Unlock()
	if v.BtOut >= len(layers) { // a pair this model has no name for (hand-edited file, or another model's)
		v.BtOut = 0
	}
	respond(w, bluetoothStatus{AssignLayer: layers[v.BtOut], Layers: layers})
}
