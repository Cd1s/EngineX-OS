package compat

import (
	"context"
	"errors"
	"testing"
)

func TestIssue233ResolverStopsCanceledWalk(t *testing.T) {
	r, err := NewResolver(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.ResolveContext(ctx, "Contents/Missing/Track.mp3"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled lookup touched the directory: %v", err)
	}
}
