package webui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/stems"
)

// The deck shows the renderer a track goes to (#238 review). With a relay that
// only offers hpss and a home computer that has the neural model, tracks go to
// the computer (stemsproc: TestOnlyANeuralRendererGetsTheTrack), so the status
// must name the computer, and the relay's later answers must not put the relay
// back. A home computer that only has hpss changes nothing, and when the
// neural one goes away the relay is reported again.
func TestStemsStatusShowsTheRendererInUse(t *testing.T) {
	prevProbe, prevEvery := lanStemsProbe, lanProbeEvery
	lanProbeEvery = 5 * time.Millisecond
	t.Cleanup(func() { lanStemsProbe, lanProbeEvery = prevProbe, prevEvery })

	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz/stems" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"ok":true,"device":"relay-box","backend":"hpss"}`))
	}))
	defer relay.Close()
	var lan atomic.Pointer[stems.LANState] // what answers on the LAN, if anything
	lanStemsProbe = func(context.Context, *stems.LANState, time.Duration) (stems.LANState, bool) {
		if st := lan.Load(); st != nil {
			return *st, true
		}
		return stems.LANState{}, false
	}

	state := t.TempDir()
	s := New(Config{UIDir: t.TempDir(), StateDir: state}, nil)
	defer s.Close()
	relayURL, password := relay.URL, "pw"
	if err := s.updateRelayConfig(&relayURL, &password); err != nil {
		t.Fatal(err)
	}
	shown := func() string {
		if st := s.relayStatusView().LANStems; st != nil && st.Online {
			return st.Hostname + " " + st.Backend
		}
		return ""
	}
	// relayAnswers is the relay's next 30-second answer, now.
	relayAnswers := func() {
		relayProbeMu.Lock()
		delete(relayProbed, state)
		relayProbeMu.Unlock()
		s.refreshRelayStatus()
	}
	settles := func(want string) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for shown() != want {
			if time.Now().After(deadline) {
				t.Fatalf("the deck shows %q, want %q", shown(), want)
			}
			time.Sleep(10 * time.Millisecond)
			relayAnswers()
		}
		for i := 0; i < 5; i++ { // and it stays, through LAN probes and relay answers
			relayAnswers()
			time.Sleep(10 * time.Millisecond)
			if got := shown(); got != want {
				t.Fatalf("the deck shows %q after settling on %q", got, want)
			}
		}
	}

	settles("relay-box hpss")
	lan.Store(&stems.LANState{Endpoint: "http://192.168.1.50:17890", Hostname: "old-pc", Backend: "hpss"})
	settles("relay-box hpss")
	lan.Store(&stems.LANState{Endpoint: "http://192.168.1.51:17890", Hostname: "studio-pc", Backend: stems.Neural})
	settles("studio-pc " + stems.Neural)
	lan.Store(nil)
	settles("relay-box hpss")
}

var consoleTableStartRE = regexp.MustCompile(`const\s+T\s*=\s*\{`)

func extractConsoleTable(page string) (string, error) {
	loc := consoleTableStartRE.FindStringIndex(page)
	if loc == nil {
		return "", errors.New("missing translation table start in index.html")
	}
	tStart := loc[0]
	relEnd := strings.Index(page[tStart:], "\n};\n")
	if relEnd < 0 {
		return "", errors.New("missing translation table end in index.html")
	}
	return page[tStart : tStart+relEnd+4], nil
}

// #238 / #336 E3: the web console's Stems status line must agree with
// stems-processor and the on-deck status line across all four languages:
// only htdemucs is reported as high-quality remote separation; hpss or an
// unnamed backend is reported as unused with on-deck basic quality; an
// unavailable (busy or unreachable) named renderer uses neutral unavailable
// wording instead of "offline"; a standalone deck states on-deck basic
// quality and recommends LAN htdemucs; AZ04 units (capabilities.remoteStems
// === false) omit the suffix; and the Relay card refreshes live while open
// without clobbering unsaved inputs.
func TestConsoleStemsStatusMatchesIssue238(t *testing.T) {
	if _, err := extractConsoleTable("const T = {\na:['1','2','3','4']\n};\n"); err != nil {
		t.Fatalf("extractConsoleTable rejected spaced table marker: %v", err)
	}
	if _, err := extractConsoleTable("no translation table here"); err == nil {
		t.Fatal("extractConsoleTable accepted input without translation table")
	}

	b, err := assets.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(b)
	for _, want := range []string{
		`id="relayCard"`,
		"function stemsLine(st,remote)",
		"st.backend==='htdemucs'",
		"capabilities.remoteStems",
		"stemsHq:[",
		"stemsHpss:[",
		"stemsDeckUnavail:[",
		"stemsDeck:[",
		"function syncRelayPoll()",
		"$('relayCard').ontoggle=syncRelayPoll",
		"relayUrlDirty",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("pkg/webui/index.html missing %q", want)
		}
	}
	for _, stale := range []string{"lanOn:[", "lanOff:[", "stemsDeckOff:["} {
		if strings.Contains(page, stale) {
			t.Fatalf("pkg/webui/index.html still contains stale %q", stale)
		}
	}

	t.Run("node_runtime", func(t *testing.T) {
		node, err := exec.LookPath("node")
		if err != nil {
			t.Skipf("node unavailable: %v", err)
		}
		tTable, err := extractConsoleTable(page)
		if err != nil {
			t.Fatal(err)
		}
		tFn := regexp.MustCompile(`(?m)^function t\(k,\.\.\.a\)\{[^\n]+`).FindString(page)
		stemsFn := regexp.MustCompile(`(?m)^function stemsLine\([^\n]+`).FindString(page)
		if tFn == "" || stemsFn == "" {
			t.Fatalf("could not extract t/stemsLine from index.html")
		}
		script := `
const assert = require('assert');
` + tTable + `
const LANGS = ['en', 'zh-Hans', 'zh-Hant', 'es'];
let lang = 'en';
` + tFn + `
` + stemsFn + `
for (const [l, unavailWord] of [
  ['en', 'unavailable'],
  ['zh-Hans', '暂不可用'],
  ['zh-Hant', '暫不可用'],
  ['es', 'no disponible'],
]) {
  lang = l;
  assert.ok(stemsLine({online: true, hostname: 'studio-pc', backend: 'htdemucs'}, true).includes('studio-pc'));
  assert.ok(stemsLine({online: true, hostname: 'studio-pc', backend: 'htdemucs'}, true).includes('htdemucs'));
  const hpss = stemsLine({online: true, hostname: 'relay-box', backend: 'hpss', relay: true}, true);
  assert.ok(hpss.includes('relay-box') && hpss.includes('hpss') && hpss.includes('htdemucs'), l + ': ' + hpss);
  assert.ok(!/online|已连上|已連上|en línea/i.test(hpss), l + ' falsely marks hpss online: ' + hpss);
  const unavail = stemsLine({online: false, hostname: 'studio-pc', backend: 'htdemucs'}, true);
  assert.ok(unavail.includes('studio-pc') && unavail.includes(unavailWord) && unavail.includes('htdemucs'), l + ': ' + unavail);
  assert.ok(!/offline|离线|離線|desconectado/i.test(unavail), l + ' mislabels unavailable renderer as offline: ' + unavail);
  assert.equal(stemsLine({online: true, hostname: 'studio-pc', backend: 'htdemucs'}, false), '');
  assert.ok(stemsLine(null, true).includes('htdemucs'), l + ' standalone missing htdemucs hint');
  assert.ok(stemsLine({online: true, endpoint: 'http://192.168.1.9:17890'}, true).includes('http://192.168.1.9:17890'));
}
`
		if out, err := exec.Command(node, "-e", script).CombinedOutput(); err != nil {
			t.Fatalf("node stemsLine check failed: %v\n%s", err, out)
		}
	})
}

// #336 E3 QA Finding 2: probeRelayStems maps a healthy HTTP 200 busy=true reply
// to Online=false; the Web Console must label that reachable renderer with
// neutral "unavailable" wording (never "offline") in all four languages while
// keeping the on-deck basic-quality fallback and LAN htdemucs guidance.
func TestReviewBusyRendererIsReachable(t *testing.T) {
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ok":true,"busy":true,"device":"B","backend":"htdemucs"}`)
	}))
	defer relay.Close()
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	defer s.Close()
	st := s.probeRelayStems(relay.URL, "fixture", nil)
	if st == nil || st.Hostname != "B" || st.Online {
		t.Fatalf("expected reachable busy health reply with Online=false, got %+v", st)
	}
	b, err := assets.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(b)
	tTable, err := extractConsoleTable(page)
	if err != nil {
		t.Fatal(err)
	}
	tFn := regexp.MustCompile(`(?m)^function t\(k,\.\.\.a\)\{[^\n]+`).FindString(page)
	stemsFn := regexp.MustCompile(`(?m)^function stemsLine\([^\n]+`).FindString(page)
	if tFn == "" || stemsFn == "" {
		t.Fatal("could not extract t/stemsLine from index.html")
	}
	state, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node unavailable: %v", err)
	}
	js := tTable + "\nconst LANGS=['en','zh-Hans','zh-Hant','es'];let lang='en';\n" +
		tFn + "\n" + stemsFn + `
const st = JSON.parse(process.argv[1]);
const out = {};
for (const l of LANGS) {
  lang = l;
  out[l] = stemsLine(st, true);
}
console.log(JSON.stringify(out));
`
	raw, err := exec.Command(node, "-e", js, string(state)).CombinedOutput()
	if err != nil {
		t.Fatalf("node failed: %v\n%s", err, raw)
	}
	var got map[string]string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("bad node JSON %q: %v", raw, err)
	}
	wantWords := map[string]struct {
		need, forbid string
	}{
		"en":      {"unavailable", "offline"},
		"zh-Hans": {"暂不可用", "离线"},
		"zh-Hant": {"暫不可用", "離線"},
		"es":      {"no disponible", "desconectado"},
	}
	for l, check := range wantWords {
		line := got[l]
		if strings.Contains(strings.ToLower(line), check.forbid) {
			t.Fatalf("%s health=200 busy=true, Online=%t mislabelled %q: %q", l, st.Online, check.forbid, line)
		}
		if !strings.Contains(line, "B") || !strings.Contains(line, check.need) || !strings.Contains(line, "htdemucs") {
			t.Fatalf("%s busy status missing expected guidance (%q): %q", l, check.need, line)
		}
	}
}
