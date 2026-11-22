package midimapper

import "testing"

// The list of batches waiting for an acknowledgement stays bounded for a caller that never sends one.
func TestInflightBatchesStayBoundedWithoutAck(t *testing.T) {
	f := testMapper(t)
	f.enabled = true
	for i := 0; i < 1000; i++ {
		f.mu.Lock()
		f.pendingValues["cueHold"] = true
		f.mu.Unlock()
		_ = action(t, f, object{"action": "poll"})
	}
	f.mu.Lock()
	n := len(f.inflightBatches)
	f.mu.Unlock()
	if n > 65 {
		t.Fatalf("%d batches wait for an acknowledgement that never comes; the bound is 64", n)
	}
}
