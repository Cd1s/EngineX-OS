package stems

import (
	"context"
	"time"
)

// ProbeLAN checks an existing home Stems renderer (if any) and otherwise probes
// the LAN once without touching disk when nothing answers, so background UI
// discovery never churns stems-lan.json on idle standalone decks (BH2-A9-1).
// It chooses as stems-processor does (chooseLAN), so the deck shows the
// renderer a track would go to.
func ProbeLAN(ctx context.Context, last *LANState, timeout time.Duration) (LANState, bool) {
	cached := ""
	if last != nil && last.Endpoint != "" && validEndpoint(last.Endpoint) {
		cached = last.Endpoint
	}
	return chooseLAN(ctx, cached, nil, timeout)
}
