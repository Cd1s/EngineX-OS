package rekordbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestReadAnalysisFilePacedChunksAndStops(t *testing.T) {
	// Build a >150 KiB PWV6 ANLZ file so 64 KiB chunks exercise multiple reads.
	entries := make([][3]byte, 60000)
	for i := range entries {
		entries[i] = [3]byte{byte(i % 127), byte((i * 3) % 127), byte((i * 5) % 127)}
	}
	raw := buildTestPWV6ANLZ(entries)
	p := filepath.Join(t.TempDir(), "ANLZ0000.2EX")
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	want, err := ReadAnalysis(p)
	if err != nil {
		t.Fatal(err)
	}

	const chunk = 64 << 10
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var chunks []int
	got, _, err := ReadAnalysisFilePaced(f, nil, chunk, func(n int, _ time.Duration) error {
		if n > chunk {
			t.Fatalf("read chunk %d exceeds %d", n, chunk)
		}
		chunks = append(chunks, n)
		return nil
	})
	if err != nil {
		t.Fatalf("ReadAnalysisFilePaced: %v", err)
	}
	if len(chunks) < 3 {
		t.Fatalf("expected >=3 chunks for %d-byte file, got %v", len(raw), chunks)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatal("paced analysis result differs from ReadAnalysis")
	}

	// Stopping on first chunk propagates error immediately.
	calls := 0
	_, _, err = ReadAnalysisFilePaced(f, nil, chunk, func(int, time.Duration) error {
		calls++
		return context.Canceled
	})
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("stop err=%v calls=%d, want context.Canceled and 1", err, calls)
	}
}
