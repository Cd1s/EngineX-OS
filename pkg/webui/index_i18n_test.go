package webui

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// #67: every string on /ui comes from the three-language table T, and the page
// has no hard-coded Chinese outside it (the language switch names excepted).
func TestConsoleIsFullyTrilingual(t *testing.T) {
	b, err := assets.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(b)
	start, end := strings.Index(page, "const T={"), strings.Index(page, "\n};\n")
	if start < 0 || end < start {
		t.Fatal("no translation table")
	}
	table := page[start:end]
	entries := map[string]int{}
	for _, m := range regexp.MustCompile(`(\w+):\[('(?:[^'\\]|\\.)*'),('(?:[^'\\]|\\.)*'),('(?:[^'\\]|\\.)*')\]`).FindAllStringSubmatch(table, -1) {
		n := 0
		for _, s := range m[2:] {
			if len(s) > 2 {
				n++
			}
		}
		entries[m[1]] = n
	}
	used := map[string]bool{}
	for _, re := range []string{`data-i18n(?:-ph|-aria|-alt)?="(\w+)"`, `\bt\('(\w+)'`} {
		for _, m := range regexp.MustCompile(re).FindAllStringSubmatch(page, -1) {
			used[m[1]] = true
		}
	}
	for _, key := range []string{"jQueued", "jMatching", "jDownloading", "jAnalyzing", "jImporting", "jDone", "jError", "kInternal", "kHd", "kSd", "kUsb", "kDrive"} {
		used[key] = true // looked up through a map, not a literal t('...')
	}
	if len(used) < 60 {
		t.Fatalf("found only %d keys; did the markup change?", len(used))
	}
	for key := range used {
		if entries[key] != 3 {
			t.Errorf("%s: %d of 3 languages", key, entries[key])
		}
	}
	rest := page[:start] + page[end:]
	for _, name := range []string{"简体中文", "繁體中文"} {
		rest = strings.ReplaceAll(rest, name, "")
	}
	if m := regexp.MustCompile(`\p{Han}+`).FindString(rest); m != "" {
		t.Errorf("untranslated text outside the table: %q", m)
	}
}

// #67: service icons need no console login, prefer the UI pack's PNG and serve
// nothing but the known services.
func TestServiceIcons(t *testing.T) {
	uiDir := t.TempDir()
	ui := New(Config{AccessToken: "c", UIDir: uiDir, StateDir: t.TempDir()}, nil)
	get := func(path string) (int, string) {
		w := httptest.NewRecorder()
		ui.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		return w.Code, w.Header().Get("Content-Type")
	}
	for id := range serviceIcons {
		if code, ctype := get("/icons/" + id); code != 200 || ctype != "image/svg+xml" {
			t.Errorf("%s: %d %s", id, code, ctype)
		}
	}
	os.MkdirAll(filepath.Join(uiDir, "icons"), 0o755)
	os.WriteFile(filepath.Join(uiDir, "icons", "spotify.png"), []byte("\x89PNG"), 0o644)
	if code, ctype := get("/icons/spotify"); code != 200 || ctype != "image/png" {
		t.Fatalf("pack icon not preferred: %d %s", code, ctype)
	}
	for _, bad := range []string{"/icons/..%2Fsettings.conf", "/icons/settings.conf"} {
		if code, _ := get(bad); code == 200 {
			t.Errorf("%s served", bad)
		}
	}
}
