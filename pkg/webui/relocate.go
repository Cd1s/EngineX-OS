package webui

// Relocate Missing Tracks (#74): POST /api/relocate (console login) and
// POST /v1/enginex/relocate (the deck itself). A request that does not say
// dryRun:false only previews; applying backs m.db up first (enginedb.Relocate).

import (
	"errors"
	"net/http"

	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

type relocateRequest struct {
	DryRun *bool  `json:"dryRun"`
	Drive  string `json:"drive"` // one drive by name; empty means every drive with an Engine Library
}

type relocateDrive struct {
	Name string `json:"name"`
	enginedb.RelocateResult
	Error string `json:"error,omitempty"`
}

func (s *Server) relocate(w http.ResponseWriter, r *http.Request) {
	var in relocateRequest
	if !input(w, r, &in) {
		return
	}
	dry := in.DryRun == nil || *in.DryRun
	out := []relocateDrive{}
	for _, d := range s.drives() {
		if in.Drive != "" && in.Drive != d.name {
			continue
		}
		res, err := enginedb.Relocate(r.Context(), enginedb.RelocateOptions{Root: d.root, DryRun: dry})
		item := relocateDrive{Name: d.name, RelocateResult: res}
		if err != nil {
			item.Error = err.Error()
		}
		out = append(out, item)
	}
	if in.Drive != "" && len(out) == 0 {
		fail(w, 404, errors.New("no Engine Library on that drive"))
		return
	}
	respond(w, map[string]any{"dryRun": dry, "drives": out})
}
