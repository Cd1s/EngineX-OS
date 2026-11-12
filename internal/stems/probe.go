package stems

import (
	"context"
	"time"
)

// ProbeLAN checks an existing home Stems renderer (if any) and otherwise probes
// the LAN once without touching disk when nothing answers, so background UI
// discovery never churns stems-lan.json on idle standalone decks (BH2-A9-1).
func ProbeLAN(ctx context.Context, last *LANState, timeout time.Duration) (LANState, bool) {
	if last != nil && last.Endpoint != "" && validEndpoint(last.Endpoint) {
		if st, ok := lanHealth(ctx, last.Endpoint, timeout); ok {
			return st, true
		}
	}
	endpoint, _, err := DiscoverLANServer(ctx, timeout)
	if err != nil {
		return LANState{}, false
	}
	return lanHealth(ctx, endpoint, timeout)
}
