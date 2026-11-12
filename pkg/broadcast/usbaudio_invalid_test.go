package broadcast

import (
	"errors"
	"testing"
)

// The Web Console answers 400 only for a configuration Apply refuses; a failure to apply is 500.
func TestUSBAudioInvalidConfigIsMarked(t *testing.T) {
	mgr := NewUSBAudioManager("", "/nonexistent-helper", "/nonexistent-cards")
	for _, cfg := range []USBAudioConfig{{HostDACMode: "bad"}, {SampleRate: 12345}} {
		if err := mgr.Apply(cfg); !errors.Is(err, ErrInvalidUSBAudioConfig) {
			t.Errorf("Apply(%+v) = %v, want ErrInvalidUSBAudioConfig", cfg, err)
		}
	}
}
