package webui

import (
	"strings"
	"testing"

	"github.com/Cd1s/EngineX-OS/pkg/providers"
)

// A streamed track reaches Engine without the service's tempo, so Engine analyses it as it
// does the other unit's shadow copy of it and both show one BPM and grid (2026-11-20: RIGHT
// showed NetEase's 84.00 for Supermanera, LEFT's copy Engine's 140.00). The row label keeps
// the service's tempo; a rekordbox Link track keeps its BPM, its analysis is replayed.
func TestStreamedTracksReachEngineWithoutTheServiceBPM(t *testing.T) {
	g := &tidalGateway{}
	for _, provider := range []string{"netease", "deezer", "spotify", "applemusic", "beatport", "soundcloud"} {
		v := g.trackJSON(1, providers.Track{Provider: provider, Title: "Supermanera", BPM: 84})
		if v["bpm"] != 0 {
			t.Errorf("%s: Engine is given bpm %v", provider, v["bpm"])
		}
		if label, _ := v["copyright"].(string); !strings.Contains(label, "84 BPM") {
			t.Errorf("%s: label %q lost the service's tempo", provider, label)
		}
	}
	if v := g.trackJSON(2, providers.Track{Provider: rekordboxScope, Title: "Fons et Origo", BPM: 129.5}); v["bpm"] != 129.5 {
		t.Errorf("rekordbox: bpm %v, want 129.5", v["bpm"])
	}
}
