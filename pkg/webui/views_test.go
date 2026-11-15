package webui

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// #99: the active view and each view's layout survive a restart; bad input does not land.
func TestViewsRoundTrip(t *testing.T) {
	s := New(Config{UIDir: t.TempDir(), StateDir: t.TempDir()}, nil)
	get := func() string {
		w := httptest.NewRecorder()
		s.viewsGet(w, httptest.NewRequest("GET", "/v1/enginex/views", nil))
		return strings.TrimSpace(w.Body.String())
	}
	if got := get(); got != `{"active":1,"views":{}}` {
		t.Fatalf("no file: %s", got)
	}
	post := func(body string) int {
		w := httptest.NewRecorder()
		s.viewsPost(w, httptest.NewRequest("POST", "/v1/enginex/views", strings.NewReader(body)))
		return w.Code
	}
	good := `{"active":4,"views":{"4":{"deckCount":"4 Decks","pads":false}}}`
	if code := post(good); code != 200 {
		t.Fatalf("save: %d", code)
	}
	for _, bad := range []string{
		`{"active":6,"views":{}}`,
		`{"active":1,"views":{"01":{}}}`,
		`{"active":1,"views":{"2":{"pads":1}}}`,
		`{"active":1,"views":{"2":{"x":"` + strings.Repeat("a", 65) + `"}}}`,
		`not json`,
	} {
		if code := post(bad); code != 400 {
			t.Errorf("%s: %d, want 400", bad, code)
		}
	}
	if got := get(); got != good {
		t.Fatalf("after restart: %s", got)
	}
}
