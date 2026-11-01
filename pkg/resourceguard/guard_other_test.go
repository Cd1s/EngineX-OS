//go:build !linux

package resourceguard

import (
	"context"
	"testing"
)

func TestPortableDisabledGuardAllowsDevelopment(t *testing.T) {
	guard := New(Config{Disabled: true})
	reports, err := guard.ApplyToProcess(0)
	if err != nil || len(reports) == 0 {
		t.Fatal("disabled process guard failed", reports, err)
	}
	ran := false
	if err = guard.Run(context.Background(), func(*Throttle) error { ran = true; return nil }); err != nil || !ran {
		t.Fatal("disabled guard did not run work", err)
	}
	if reports[0].SchedIdle || reports[0].IOIdle {
		t.Fatal("disabled guard applied controls")
	}
}

func TestPortableEnabledGuardReportsUnsupported(t *testing.T) {
	if _, err := New(Config{}).ApplyToProcess(0); err == nil {
		t.Fatal("unsupported process controls silently succeeded")
	}
}
