package stems

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestResolveClientFromRelayConfig(t *testing.T) {
	dir := t.TempDir()
	cfg := writeFile(t, dir, "deemix-config.json", `{"relayUrl":"https://relay.example/sc5000/","deemixPassword":" console-pass "}`)
	c, err := ResolveClient(envOf(map[string]string{"SC5000_STEMS_CONFIG": cfg}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Endpoint != "https://relay.example/sc5000/" || c.Secret != "console-pass" || c.Source != cfg {
		t.Fatalf("%+v", c)
	}
	// SC5000_STEMS_TOKEN replaces only the password.
	c, err = ResolveClient(envOf(map[string]string{"SC5000_STEMS_CONFIG": cfg, "SC5000_STEMS_TOKEN": "other"}))
	if err != nil || c.Endpoint != "https://relay.example/sc5000/" || c.Secret != "other" {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestResolveClientDefaultsAndLegacyKey(t *testing.T) {
	dir := t.TempDir()
	cfg := writeFile(t, dir, "a.json", `{"deemixPassword":"pw"}`)
	if c, err := ResolveClient(envOf(map[string]string{"SC5000_STEMS_CONFIG": cfg})); err != nil || c.Endpoint != "" {
		t.Fatalf("a password without relayUrl must mean standalone: %+v %v", c, err)
	}
	cfg = writeFile(t, dir, "b.json", `{"deemixUrl":"http://192.168.1.5:6595","deemixPassword":"pw"}`)
	c, err := ResolveClient(envOf(map[string]string{"SC5000_STEMS_CONFIG": cfg}))
	if err != nil || c.Endpoint != "http://192.168.1.5:6595" {
		t.Fatalf("legacy deemixUrl: %+v %v", c, err)
	}
}

func TestResolveClientRejects(t *testing.T) {
	dir := t.TempDir()
	for name, c := range map[string]struct{ body, want string }{
		"plain http": {`{"relayUrl":"http://relay.example","deemixPassword":"pw"}`, "https"},
		"bad json":   {`{`, "invalid JSON"},
	} {
		cfg := writeFile(t, dir, strings.ReplaceAll(name, " ", "-")+".json", c.body)
		_, err := ResolveClient(envOf(map[string]string{"SC5000_STEMS_CONFIG": cfg}))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
		if err != nil && strings.Contains(err.Error(), "pw") {
			t.Errorf("%s: error leaks the password: %v", name, err)
		}
	}
}

// Without a renderer set up the deck renders on its own.
func TestResolveClientStandalone(t *testing.T) {
	dir := t.TempDir()
	for _, cfg := range []string{filepath.Join(dir, "missing.json"), writeFile(t, dir, "no-password.json", `{"relayUrl":"https://relay.example"}`)} {
		c, err := ResolveClient(envOf(map[string]string{"SC5000_STEMS_CONFIG": cfg}))
		if err != nil || c.Endpoint != "" || c.Source != "standalone" {
			t.Errorf("%s: %+v %v", cfg, c, err)
		}
	}
}

func TestResolveClientDirectOverride(t *testing.T) {
	dir := t.TempDir()
	token := writeFile(t, dir, "token", strings.Repeat("t", 32)+"\n")
	// The relay config is ignored once SC5000_STEMS_URL is set.
	cfg := writeFile(t, dir, "deemix-config.json", `{"deemixPassword":"console"}`)
	env := map[string]string{"SC5000_STEMS_URL": "http://192.168.6.104:6596", "SC5000_STEMS_TOKEN_FILE": token, "SC5000_STEMS_CONFIG": cfg}
	c, err := ResolveClient(envOf(env))
	if err != nil || c.Endpoint != "http://192.168.6.104:6596" || c.Secret != strings.Repeat("t", 32) {
		t.Fatalf("%+v %v", c, err)
	}
	env["SC5000_STEMS_TOKEN"] = "inline"
	if c, err = ResolveClient(envOf(env)); err != nil || c.Secret != "inline" {
		t.Fatalf("token override: %+v %v", c, err)
	}
	for _, bad := range []string{"http://8.8.8.8:6596", "ftp://192.168.1.2", "https://u:p@host"} {
		if _, err := ResolveClient(envOf(map[string]string{"SC5000_STEMS_URL": bad, "SC5000_STEMS_TOKEN": "x"})); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	if _, err := ResolveClient(envOf(map[string]string{"SC5000_STEMS_URL": "http://100.100.1.2:6596", "SC5000_STEMS_TOKEN": "x"})); err != nil {
		t.Errorf("tailnet address rejected: %v", err)
	}
	// Short direct-mode token files are still refused.
	short := writeFile(t, dir, "short", "abc")
	if _, err := ResolveClient(envOf(map[string]string{"SC5000_STEMS_URL": "http://127.0.0.1:6596", "SC5000_STEMS_TOKEN_FILE": short})); err == nil {
		t.Error("short token accepted")
	}
}

func TestResolveClientAcceptsMDNSAndLANHostnames(t *testing.T) {
	for _, good := range []string{
		"http://studio-mac.local:18765",
		"http://deck-worker.lan:18765",
		"http://localhost:6596",
		"http://renderbox:18765",
	} {
		c, err := ResolveClient(envOf(map[string]string{"SC5000_STEMS_URL": good, "SC5000_STEMS_TOKEN": "tok"}))
		if err != nil || c.Endpoint != good {
			t.Fatalf("ResolveClient(%q) = (%+v, %v), want accepted", good, c, err)
		}
	}
	for _, bad := range []string{
		"http://.local:18765",
		"http://.lan:18765",
		"http://relay.example:18765",
	} {
		if _, err := ResolveClient(envOf(map[string]string{"SC5000_STEMS_URL": bad, "SC5000_STEMS_TOKEN": "tok"})); err == nil {
			t.Fatalf("ResolveClient(%q) accepted non-LAN HTTP host", bad)
		}
	}
}
