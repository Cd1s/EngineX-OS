package webui

import (
	"strings"
	"testing"
)

// The rekordbox analysis cache is the deck's /run/enginex-rbx-analysis-v3. A trace of this package's
// tests on a build host found 38 paths written there (2026-11-22): TestMain points the directory into
// its private one, and a test that needs its own sets it and puts the old value back.
func TestAnalysisCacheStaysOffTheHost(t *testing.T) {
	if rbxAnalysisDir == "/run/enginex-rbx-analysis-v3" || strings.HasPrefix(rbxAnalysisDir, "/run/") {
		t.Fatalf("rbxAnalysisDir = %s: this package's tests would write the host's /run", rbxAnalysisDir)
	}
}
