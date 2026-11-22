package webui

import (
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/mapping"
)

// The Buttons page, in a real browser, against the real handlers and a fake ALSA client.
// It runs where a Playwright install and a Chromium are named:
//
//	ENGINEX_PLAYWRIGHT_DIR=<a node_modules dir holding playwright-core> ENGINEX_CHROME=<chrome binary>
//	ENGINEX_PAGE_SHOTS=<dir> (optional: screenshots of the page)
func TestMappingPageInABrowser(t *testing.T) {
	modules, chrome := os.Getenv("ENGINEX_PLAYWRIGHT_DIR"), os.Getenv("ENGINEX_CHROME")
	if modules == "" || chrome == "" {
		t.Skip("ENGINEX_PLAYWRIGHT_DIR and ENGINEX_CHROME are not set")
	}
	mappingDeck(t, "JP07")
	var restarts atomic.Int32
	src := newLearnSource()
	s := mappingServer(t, Config{
		MappingLearnWindow: 20 * time.Second,
		RestartEngine:      func() error { restarts.Add(1); return nil },
		MappingSource:      func() (mapping.Source, error) { return src, nil },
	})
	ts := httptest.NewServer(s)
	defer ts.Close()
	// What the controller plays, once per learn: Shift + Loop, then Back (a reserved control), then nothing.
	script := [][]mapping.Event{
		{{Note: 26, On: true}, {Note: 30, On: true}, {Note: 30}, {Note: 26}},
		{{Note: 16, On: true}, {Note: 16}},
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		for run := 1; run <= len(script); run++ {
			for {
				select {
				case <-done:
					return
				case <-time.After(20 * time.Millisecond):
				}
				if subs, _ := src.counts(); subs >= run {
					break
				}
			}
			time.Sleep(150 * time.Millisecond)
			for _, e := range script[run-1] {
				src.events <- []mapping.Event{e}
			}
		}
	}()
	shots := os.Getenv("ENGINEX_PAGE_SHOTS")
	if shots == "" {
		shots = "-"
	}
	cmd := exec.Command("node", "testdata/mapping_page_check.js", ts.URL, "t", chrome, shots)
	cmd.Env = append(os.Environ(), "NODE_PATH="+modules)
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "page ok") {
		t.Fatalf("%v\n%s", err, out)
	}
	if restarts.Load() != 1 {
		t.Errorf("Engine restarted %d times from the page", restarts.Load())
	}
	if subs, closed := src.counts(); subs < 3 || closed < 3 {
		t.Errorf("subscribed %d, closed %d: every learn closes its subscription", subs, closed)
	}
}
