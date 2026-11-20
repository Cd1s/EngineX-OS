package webui

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The source's phase meter rides the peer picture: a shadow header turns green
// with its source (the copy is out of Sync, so Engine never aligns it there).
func TestShadowPhaseMeterRidesThePeerPicture(t *testing.T) {
	var d deckState
	if err := json.Unmarshal([]byte(`{"deck":2,"playing":false,"positionMs":1000,"aligned":true,"phaseActive":true,"phaseMeter":true}`), &d); err != nil {
		t.Fatal(err)
	}
	if d.Aligned == nil || !*d.Aligned || d.PhaseActive == nil || !*d.PhaseActive || d.PhaseMeter == nil || !*d.PhaseMeter {
		t.Fatalf("phase meter state lost on the way in: %+v", d)
	}
	out, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{`"aligned":true`, `"phaseActive":true`, `"phaseMeter":true`} {
		if !strings.Contains(string(out), k) {
			t.Fatalf("%s missing on the way out: %s", k, out)
		}
	}
	now := time.Now()
	old := peerDeck{deckState: d, anchorPos: d.PositionMS, anchorAt: now}
	if !samePeerPicture(old, d, now) {
		t.Fatal("an unchanged deck must keep its picture")
	}
	f := false
	for name, change := range map[string]func(*deckState){
		"aligned":     func(n *deckState) { n.Aligned = &f },
		"phaseActive": func(n *deckState) { n.PhaseActive = &f },
		"phaseMeter":  func(n *deckState) { n.PhaseMeter = &f },
	} {
		n := d
		change(&n)
		if samePeerPicture(old, n, now) {
			t.Fatalf("a change of %s alone must reach the peer's shadow header", name)
		}
	}
	var legacy deckState
	if err := json.Unmarshal([]byte(`{"deck":2,"playing":false}`), &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.Aligned != nil || legacy.PhaseActive != nil || legacy.PhaseMeter != nil {
		t.Fatal("an older sender leaves them unset: the receiver's shadow deck keeps its own meter")
	}
	if b, _ := json.Marshal(legacy); strings.Contains(string(b), "aligned") || strings.Contains(string(b), "phase") {
		t.Fatalf("unset fields must stay off the wire: %s", b)
	}
}
