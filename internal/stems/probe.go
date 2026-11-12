package stems

import (
	"context"
	"os"
	"testing"
	"time"
)

// DefaultLANStatePath prefers /data/enginex/state on migrated or fresh Gen-2
// installs, falling back to DefaultLANState (/data/sc5000-custom/state).
func DefaultLANStatePath() string {
	if !testing.Testing() {
		if st, err := os.Stat("/data/enginex/state"); err == nil && st.IsDir() {
			return "/data/enginex/state/" + LANStateName
		}
	}
	return DefaultLANState
}

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
