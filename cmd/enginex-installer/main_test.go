package main

// End-to-end tests (#88). They use real sockets, a real SSH server, a real
// Data-Bundle.tar.gz built by tools/az01-packer/build-data-bundle.py and real
// ext4 images checked with e2fsck/debugfs. Run them on Oracle-Batam-arm as root
// (unshare -m keeps boot-activate.sh off the host); anything missing is a SKIP,
// so read the -v output.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"

	"golang.org/x/crypto/ssh"
)

func need(t *testing.T, tools ...string) {
	t.Helper()
	for _, tool := range tools {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
	if os.Geteuid() != 0 {
		t.Skip("needs root (unshare -m, mke2fs images)")
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	d, _ := os.Getwd()
	for ; d != "/"; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
	}
	t.Fatal("go.mod not found")
	return ""
}

func fakeELF(machine byte, tag string) []byte {
	b := make([]byte, 64)
	copy(b, "\x7fELF")
	b[18] = machine
	return append(b, []byte(tag)...)
}

type bundleFile struct{ Path, SuitePath, Version string }

// buildBundle runs the real packer.
func buildBundle(t *testing.T, prefix, version string) bundleFile {
	t.Helper()
	need(t, "python3")
	dir := t.TempDir()
	machine := byte(40) // EM_ARM
	if prefix == "PRIME4G2" || prefix == "SYSTEMONE" {
		machine = 183
	}
	suite, hook := filepath.Join(dir, "sc5000-suite"), filepath.Join(dir, "libengine-ui-hook.so")
	os.WriteFile(suite, fakeELF(machine, "suite-"+prefix), 0o755)
	os.WriteFile(hook, fakeELF(machine, "hook-"+prefix), 0o755)
	ui := filepath.Join(dir, "packs.tar")
	if out, err := exec.Command("sh", "-c", "mkdir -p '"+dir+"/ui/packs/a' && echo 'Item {}' > '"+dir+"/ui/packs/a/Main.qml' && tar -C '"+dir+"/ui' -cf '"+ui+"' packs").CombinedOutput(); err != nil {
		t.Fatalf("ui packs: %v %s", err, out)
	}
	out := filepath.Join(dir, "out")
	script := filepath.Join(repoRoot(t), "tools/az01-packer/build-data-bundle.py")
	if o, err := exec.Command("python3", "-I", script, version, "--suite", suite, "--hook", hook, "--ui-packs", ui, "--out", out, "--prefix", prefix).CombinedOutput(); err != nil {
		t.Fatalf("build-data-bundle: %v %s", err, o)
	}
	return bundleFile{filepath.Join(out, prefix+"-"+version+"-EngineX-OS-Data-Bundle.tar.gz"), suite, version}
}

// corrupt rewrites the bundle with one SHA256SUMS digest altered.
func corrupt(t *testing.T, b bundleFile) bundleFile {
	t.Helper()
	dir := t.TempDir()
	x := filepath.Join(dir, "x")
	os.Mkdir(x, 0o755)
	if o, err := exec.Command("tar", "-xzf", b.Path, "-C", x).CombinedOutput(); err != nil {
		t.Fatalf("tar: %v %s", err, o)
	}
	top := strings.TrimSuffix(filepath.Base(b.Path), ".tar.gz")
	sums := filepath.Join(x, top, "SHA256SUMS")
	data, _ := os.ReadFile(sums)
	os.WriteFile(sums, append([]byte("0"), data[1:]...), 0o644)
	out := filepath.Join(dir, filepath.Base(b.Path))
	if o, err := exec.Command("tar", "-czf", out, "-C", x, top).CombinedOutput(); err != nil {
		t.Fatalf("tar: %v %s", err, o)
	}
	return bundleFile{out, b.SuitePath, b.Version}
}

func sha(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// releaseServer is a TLS GitHub: releases/latest plus the bundle download.
func releaseServer(t *testing.T, b bundleFile, digest string, downloads *int) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	mux := http.NewServeMux()
	name := filepath.Base(b.Path)
	if digest == "" {
		digest = "sha256:" + sha(t, b.Path)
	}
	mux.HandleFunc("/repos/Cd1s/EngineX-OS/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"tag_name": "v" + b.Version, "assets": []map[string]string{
			{"name": "SC5000-" + b.Version + "-EngineX-OS-Denon-SC5000-Prime-JP07-Update.img", "browser_download_url": srv.URL + "/dl/img", "digest": "sha256:00"},
			{"name": name, "browser_download_url": srv.URL + "/dl/" + name, "digest": digest},
		}})
	})
	mux.HandleFunc("/dl/", func(w http.ResponseWriter, r *http.Request) {
		if downloads != nil {
			*downloads++
		}
		http.ServeFile(w, r, b.Path)
	})
	srv = httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// airD builds a StageLinQ discovery packet (magic, token, four UTF-16BE strings, port).
func airD(source, action, software, version string, port uint16) []byte {
	p := append([]byte("airD"), make([]byte, 16)...)
	for _, s := range []string{source, action, software, version} {
		u := utf16.Encode([]rune(s))
		p = binary.BigEndian.AppendUint32(p, uint32(len(u)*2))
		for _, c := range u {
			p = binary.BigEndian.AppendUint16(p, c)
		}
	}
	return binary.BigEndian.AppendUint16(p, port)
}

func freeUDP(t *testing.T) int {
	t.Helper()
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	return pc.LocalAddr().(*net.UDPAddr).Port
}

func freeTCP(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// announceFrom sends pkt to udpPort from the loopback address src until the test ends.
func announceFrom(t *testing.T, src string, udpPort int, pkt []byte) {
	t.Helper()
	c, err := net.DialUDP("udp4", &net.UDPAddr{IP: net.ParseIP(src)}, &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: udpPort})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	t.Cleanup(func() { close(done); c.Close() })
	go func() {
		for {
			c.Write(pkt)
			select {
			case <-done:
				return
			case <-time.After(50 * time.Millisecond):
			}
		}
	}()
}

func testOpts(t *testing.T) *options {
	t.Helper()
	return &options{
		Repo: "Cd1s/EngineX-OS", Yes: true, Fastboot: "no-such-fastboot-binary",
		AirDAddr: fmt.Sprintf("127.0.0.1:%d", freeUDP(t)), AirDWait: 800 * time.Millisecond,
		Subnets: []string{"127.0.0"}, UIPort: freeTCP(t), SSHPort: freeTCP(t),
		WorkDir: t.TempDir(), WaitUSB: time.Second, Out: &bytes.Buffer{}, In: strings.NewReader(""),
		ListUSB: func(context.Context) string { return "" }, // never the build host's real USB bus
	}
}

func useRelease(o *options, srv *httptest.Server) { o.API, o.HTTP = srv.URL, srv.Client() }

func ctxT(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// ---- Test 1: discovery from real sockets --------------------------------

func TestDiscoverFindsDecksWithoutAnIP(t *testing.T) {
	o := testOpts(t)
	announceFrom(t, "127.0.0.1", freeUDPFrom(o), airD("prime4plus-lab", "DISCOVERER_HOWDY_", "JC11S", "2.3.4", 1234))
	announceFrom(t, "127.0.0.3", freeUDPFrom(o), airD("laptop", "DISCOVERER_HOWDY_", "Engine DJ", "4.0", 1))
	announceFrom(t, "127.0.0.4", freeUDPFrom(o), airD("gone", "DISCOVERER_EXIT_", "JC11S", "2.3.4", 1))

	// A deck's web UI on 127.0.0.2, and a web server that answers 200 to anything on .5.
	serveHTTP(t, fmt.Sprintf("127.0.0.2:%d", o.UIPort), func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/sc5000/ui" {
			w.Write([]byte(`{"ok":true}`))
			return
		}
		http.NotFound(w, r)
	})
	serveHTTP(t, fmt.Sprintf("127.0.0.5:%d", o.UIPort), func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("<html>router</html>"))
	})

	got := map[string]deck{}
	for _, d := range discover(ctxT(t), o) {
		got[d.IP] = d
	}
	if d := got["127.0.0.1"]; d.Name != "prime4plus-lab" || d.Board != "jc11s" || !d.Known || !d.Announced {
		t.Errorf("announced deck = %+v", d)
	}
	if d := got["127.0.0.2"]; !d.Known || d.Announced {
		t.Errorf("deck found by web UI = %+v", d)
	}
	if d, ok := got["127.0.0.3"]; !ok || d.Known || d.Board != "" {
		t.Errorf("unknown announcer should be listed but not Known: %+v", d)
	}
	for _, ip := range []string{"127.0.0.4", "127.0.0.5"} {
		if d, ok := got[ip]; ok {
			t.Errorf("%s must not be reported: %+v", ip, d)
		}
	}
}

func freeUDPFrom(o *options) int {
	_, p, _ := net.SplitHostPort(o.AirDAddr)
	var n int
	fmt.Sscan(p, &n)
	return n
}

func serveHTTP(t *testing.T, addr string, h http.HandlerFunc) {
	t.Helper()
	l, err := net.Listen("tcp4", addr)
	if err != nil {
		t.Skipf("cannot listen on %s: %v", addr, err)
	}
	s := &http.Server{Handler: h}
	go s.Serve(l)
	t.Cleanup(func() { s.Close() })
}

func TestParseAirDRejectsGarbage(t *testing.T) {
	good := airD("sc6000", "DISCOVERER_HOWDY_", "JP13", "1.0", 51337)
	if a, err := parseAirD(good); err != nil || a.Source != "sc6000" || a.Software != "JP13" || a.Port != 51337 {
		t.Fatalf("good packet: %+v %v", a, err)
	}
	for name, p := range map[string][]byte{
		"empty": nil, "wrong magic": append([]byte("xxxx"), good[4:]...),
		"truncated": good[:len(good)-5], "odd utf16 length": append(append([]byte("airD"), make([]byte, 16)...), 0, 0, 0, 3, 'a', 'b', 'c'),
	} {
		if _, err := parseAirD(p); err == nil {
			t.Errorf("%s: parsed without error", name)
		}
	}
}

func TestBoardTable(t *testing.T) {
	for code, prefix := range map[string]string{"jp21": "SCLIVE4", "JP20": "SCLIVE2", "jc11s": "PRIME4PLUS", "jp11s": "PRIMEGOPLUS",
		"nh08s": "MIXSTREAMPROPLUS", "NH10": "MIXSTREAMPROGO", "jp24": "PRIME4G2", "inmusic,rmz2": "SYSTEMONE",
		"rk3288-az05-jp11s": "PRIMEGOPLUS", "rk3288-az02-jp21": "SCLIVE4", "imx8-az04-jp24": "PRIME4G2",
		"inmusic,rk3288-az05-jp11s": "PRIMEGOPLUS"} {
		if m, gen2, ok := lookupBoard(code); !ok || !gen2 || m.Prefix != prefix {
			t.Errorf("%s -> %+v gen2=%v ok=%v, want %s", code, m, gen2, ok, prefix)
		}
	}
	for _, code := range []string{"jp07", "jp08", "jp11", "jp13", "jp14", "jc11", "jc16", "nh08", "rk3288-az01-jp07", "rk3288-az01-jp11"} {
		if _, gen2, ok := lookupBoard(code); !ok || gen2 {
			t.Errorf("%s should be a known first-generation board", code)
		}
	}
	for _, bad := range []string{"zz99", "rk3288-az05-zz99", "inmusic,unknown"} {
		if _, _, ok := lookupBoard(bad); ok {
			t.Errorf("%s should not be recognized", bad)
		}
	}
}

// ---- Test 2: mode A over a real SSH server --------------------------------

// sshDeck is an SSH server (password "denon") that runs each exec request with sh,
// in a private mount namespace, against a temporary /data. It answers the device-tree
// read from a fixture because the test host has no such file.
type sshDeck struct {
	Port       int
	Data       string
	mu         sync.Mutex
	cmds       []string
	compatible string
}

func (d *sshDeck) Commands() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.cmds...)
}

func startSSHDeck(t *testing.T, port int, compatible string) *sshDeck {
	t.Helper()
	need(t, "unshare")
	d := &sshDeck{Port: port, Data: t.TempDir(), compatible: compatible}
	shim := t.TempDir()
	for _, n := range []string{"systemctl", "pgrep"} { // boot-activate.sh must not touch the host
		os.WriteFile(filepath.Join(shim, n), []byte("#!/bin/sh\nexit 0\n"), 0o755)
	}
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(key)
	cfg := &ssh.ServerConfig{PasswordCallback: func(c ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
		if string(pw) == "denon" {
			return nil, nil
		}
		return nil, fmt.Errorf("denied")
	}}
	cfg.AddHostKey(signer)
	l, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	env := append(os.Environ(), "PATH="+shim+":"+os.Getenv("PATH"), "TMPDIR="+d.Data,
		"SC5000_CUSTOM="+filepath.Join(d.Data, "enginex"),
		"SC5000_ALSA_OVERLAY="+filepath.Join(d.Data, "system/var/overlay/lib/alsa/conf.d"))
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				_, chans, reqs, err := ssh.NewServerConn(c, cfg)
				if err != nil {
					return
				}
				go ssh.DiscardRequests(reqs)
				for nc := range chans {
					if nc.ChannelType() != "session" {
						nc.Reject(ssh.UnknownChannelType, "no")
						continue
					}
					ch, rq, _ := nc.Accept()
					go func() {
						for r := range rq {
							if r.Type != "exec" {
								r.Reply(false, nil)
								continue
							}
							var p struct{ Cmd string }
							ssh.Unmarshal(r.Payload, &p)
							r.Reply(true, nil)
							d.mu.Lock()
							d.cmds = append(d.cmds, p.Cmd)
							d.mu.Unlock()
							status := 0
							if p.Cmd == "cat /sys/firmware/devicetree/base/compatible" {
								io.WriteString(ch, d.compatible)
							} else if strings.Contains(p.Cmd, "/usr/sbin/az01-update --file") {
								f, err := os.Create(filepath.Join(d.Data, "az01-update.img"))
								if err != nil {
									status = 1
								} else {
									_, err = io.Copy(f, ch)
									f.Close()
									if err != nil {
										status = 1
									}
								}
							} else {
								cmd := exec.Command("unshare", "-m", "sh", "-c", `mount -t tmpfs tmpfs /run || exit 99; exec sh -c "$0"`, p.Cmd)
								cmd.Env, cmd.Stdin, cmd.Stdout, cmd.Stderr = env, ch, ch, ch.Stderr()
								if err := cmd.Run(); err != nil {
									status = 1
									if ee, ok := err.(*exec.ExitError); ok {
										status = ee.ExitCode()
									}
								}
							}
							ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(status)}))
							ch.Close()
							return
						}
					}()
				}
			}()
		}
	}()
	return d
}

func TestModeAInstallsBundleOverSSH(t *testing.T) {
	need(t, "python3", "unshare")
	b := buildBundle(t, "PRIME4PLUS", "5.3.0.2")
	downloads := 0
	rel := releaseServer(t, b, "", &downloads)
	o := testOpts(t)
	useRelease(o, rel)
	deck := startSSHDeck(t, o.SSHPort, "inmusic,jc11s\x00nxp,imx8mm\x00")
	announceFrom(t, "127.0.0.1", freeUDPFrom(o), airD("prime4plus-lab", "DISCOVERER_HOWDY_", "JC11S", "2.3.4", 1))

	if err := run(ctxT(t), o); err != nil {
		t.Fatalf("run: %v\n%s", err, o.Out)
	}
	if downloads != 1 {
		t.Errorf("bundle downloads = %d, want 1", downloads)
	}
	custom := filepath.Join(deck.Data, "enginex")
	if got, want := sha(t, filepath.Join(custom, "bundle/bin/sc5000-suite")), sha(t, b.SuitePath); got != want {
		t.Errorf("installed suite sha %s, want %s", got, want)
	}
	if v, _ := os.ReadFile(filepath.Join(custom, "version.txt")); strings.TrimSpace(string(v)) != "5.3.0.2" {
		t.Errorf("version.txt = %q", v)
	}
	if _, err := os.Stat(filepath.Join(deck.Data, "system/var/overlay/lib/alsa/conf.d/99-sc5000-custom.conf")); err != nil {
		t.Errorf("ALSA hook not installed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(custom, "bundle/boot-activate.sh")); err != nil {
		t.Errorf("boot-activate.sh missing: %v", err)
	}
	cmds := strings.Join(deck.Commands(), "\n")
	for _, want := range []string{"compatible", "tar -xzf -", "install.sh", "boot-activate.sh"} {
		if !strings.Contains(cmds, want) {
			t.Errorf("remote commands lack %q:\n%s", want, cmds)
		}
	}
	if left, _ := filepath.Glob(filepath.Join(deck.Data, "PRIME4PLUS-*")); len(left) != 0 {
		t.Errorf("unpacked bundle left in the temp dir: %v", left)
	}
}

// ---- Test 3: mode B against a fastboot shim and real ext4 images ---------------

type fbShim struct{ Bin, Dir string }

func newFastboot(t *testing.T, product string, hasData bool) fbShim {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "product"), []byte(product), 0o644)
	if !hasData {
		os.WriteFile(filepath.Join(dir, "nodata"), nil, 0o644)
	}
	script := `#!/bin/sh
d=$(dirname "$0")
echo "$*" >> "$d/calls.log"
case "$1" in
devices) printf 'LAB0001\tfastboot\n' ;;
getvar)
  case "$2" in
  product) echo "product: $(cat "$d/product")" >&2 ;;
  partition-size:data)
    if [ -f "$d/nodata" ]; then echo "getvar:partition-size:data FAILED (remote: 'GetVar Variable Not found')" >&2; exit 1; fi
    echo "partition-size:data: 0x4000000" >&2 ;;
  partition-size:userdata) echo "partition-size:userdata: 0x4000000" >&2 ;;
  *) echo "getvar:$2 FAILED (remote: 'GetVar Variable Not found')" >&2; exit 1 ;;
  esac
  echo "Finished. Total time: 0.001s" >&2 ;;
flash) echo "Sending '$2' (65536 KB)..." >&2; echo "Writing '$2'..." >&2; echo "OKAY" >&2; cp "$3" "$d/flashed-$2.img" ;;
reboot) echo "Rebooting" >&2 ;;
esac
`
	bin := filepath.Join(dir, "fastboot")
	os.WriteFile(bin, []byte(script), 0o755)
	return fbShim{bin, dir}
}

func (f fbShim) Calls() []string {
	b, _ := os.ReadFile(filepath.Join(f.Dir, "calls.log"))
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func (f fbShim) Flashed() []string {
	var out []string
	for _, c := range f.Calls() {
		if strings.HasPrefix(c, "flash ") {
			out = append(out, c)
		}
	}
	return out
}

func sh(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
	return string(out)
}

func checkImage(t *testing.T, img string, suitePath string) {
	t.Helper()
	sh(t, "e2fsck", "-fn", img) // exit code 0 or the test fails
	if hdr := sh(t, "dumpe2fs", "-h", img); !strings.Contains(hdr, "az01-data") || !strings.Contains(hdr, "encrypt") {
		t.Errorf("label/features wrong:\n%s", hdr)
	}
	st := sh(t, "debugfs", "-R", "stat /enginex/bundle/bin/sc5000-suite", img)
	for _, want := range []string{"Type: regular", "Mode:  0755", "User:     0   Group:     0"} {
		if !strings.Contains(st, want) {
			t.Errorf("suite inode lacks %q:\n%s", want, st)
		}
	}
	out := filepath.Join(t.TempDir(), "suite")
	sh(t, "debugfs", "-R", "dump /enginex/bundle/bin/sc5000-suite "+out, img)
	if sha(t, out) != sha(t, suitePath) {
		t.Error("suite in the image differs from the bundle's")
	}
	for _, p := range []string{"/system/var/overlay/lib/alsa/conf.d/99-sc5000-custom.conf", "/system/var-lib/alsa/conf.d/99-sc5000-custom.conf", "/enginex/version.txt"} {
		if s := sh(t, "debugfs", "-R", "stat "+p, img); !strings.Contains(s, "Type: regular") || !strings.Contains(s, "Mode:  0644") {
			t.Errorf("%s:\n%s", p, s)
		}
	}
}

func modeBOpts(t *testing.T, fb fbShim, b bundleFile) *options {
	o := testOpts(t)
	o.Fastboot = fb.Bin
	useRelease(o, releaseServer(t, b, "", nil))
	return o
}

func TestModeBFreshImageFlashedToData(t *testing.T) {
	need(t, "python3", "mke2fs", "debugfs", "e2fsck", "dumpe2fs")
	b := buildBundle(t, "PRIME4PLUS", "5.3.0.2")
	fb := newFastboot(t, "JC11S", true)
	o := modeBOpts(t, fb, b)
	if err := run(ctxT(t), o); err != nil {
		t.Fatalf("run: %v\n%s", err, o.Out)
	}
	flashed := fb.Flashed()
	if len(flashed) != 1 || !strings.HasPrefix(flashed[0], "flash data ") {
		t.Fatalf("flash calls = %v", flashed)
	}
	calls := fb.Calls()
	if calls[len(calls)-1] != "reboot" {
		t.Errorf("last fastboot call = %q, want reboot", calls[len(calls)-1])
	}
	checkImage(t, filepath.Join(fb.Dir, "flashed-data.img"), b.SuitePath)
}

func TestModeBRecognizesPlatformPrefixProduct(t *testing.T) {
	need(t, "python3", "mke2fs", "debugfs", "e2fsck", "dumpe2fs")
	b := buildBundle(t, "PRIMEGOPLUS", "5.3.0.2")
	fb := newFastboot(t, "rk3288-az05-jp11s", true)
	o := modeBOpts(t, fb, b)
	if err := run(ctxT(t), o); err != nil {
		t.Fatalf("run: %v\n%s", err, o.Out)
	}
	flashed := fb.Flashed()
	if len(flashed) != 1 || !strings.HasPrefix(flashed[0], "flash data ") {
		t.Fatalf("flash calls = %v", flashed)
	}
	checkImage(t, filepath.Join(fb.Dir, "flashed-data.img"), b.SuitePath)
}

func TestModeBFlashesUserdataWhenThereIsNoDataPartition(t *testing.T) {
	need(t, "python3", "mke2fs", "debugfs", "e2fsck", "dumpe2fs")
	b := buildBundle(t, "PRIME4PLUS", "5.3.0.2")
	fb := newFastboot(t, "jc11s", false)
	if err := run(ctxT(t), modeBOpts(t, fb, b)); err != nil {
		t.Fatal(err)
	}
	if f := fb.Flashed(); len(f) != 1 || !strings.HasPrefix(f[0], "flash userdata ") {
		t.Fatalf("flash calls = %v", f)
	}
	checkImage(t, filepath.Join(fb.Dir, "flashed-userdata.img"), b.SuitePath)
}

func TestModeBKeepsTheUsersFilesWhenGivenABackup(t *testing.T) {
	need(t, "python3", "mke2fs", "debugfs", "e2fsck", "dumpe2fs")
	b := buildBundle(t, "PRIME4PLUS", "5.3.0.2")
	backup := filepath.Join(t.TempDir(), "data-backup.img")
	sh(t, "truncate", "-s", "64M", backup)
	sh(t, "mke2fs", "-t", "ext4", "-O", "encrypt", "-L", "az01-data", "-F", backup)
	sh(t, "debugfs", "-w", "-R", "mkdir /Music", backup)
	sh(t, "debugfs", "-w", "-R", "write /etc/hostname /Music/keep.mp3", backup)
	fb := newFastboot(t, "JC11S", true)
	o := modeBOpts(t, fb, b)
	o.Backup = backup
	if err := run(ctxT(t), o); err != nil {
		t.Fatalf("run: %v\n%s", err, o.Out)
	}
	img := filepath.Join(fb.Dir, "flashed-data.img")
	checkImage(t, img, b.SuitePath)
	if s := sh(t, "debugfs", "-R", "stat /Music/keep.mp3", img); !strings.Contains(s, "Type: regular") {
		t.Errorf("user file lost:\n%s", s)
	}
	if sha(t, backup) == sha(t, img) {
		t.Error("the backup itself must stay untouched, not be the flashed image")
	}
	if s := sh(t, "debugfs", "-R", "stat /enginex/version.txt", backup); strings.Contains(s, "Type: regular") {
		t.Error("the backup was modified")
	}
}

// ---- Test 4: refusals ------------------------------------------------------------

func TestModeBRefusesUnknownBoards(t *testing.T) {
	need(t, "python3")
	b := buildBundle(t, "PRIME4PLUS", "5.3.0.2")
	for _, product := range []string{"zz99"} {
		fb := newFastboot(t, product, true)
		o := modeBOpts(t, fb, b)
		want := "一代机"
		if product == "zz99" {
			want = "未知"
		}
		if err := run(ctxT(t), o); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want it to say %q", product, err, want)
		}
		if c := fb.Calls(); !strings.Contains(strings.Join(c, "\n"), "getvar product") {
			t.Errorf("%s: never asked the deck what it is: %v", product, c)
		}
		if f := fb.Flashed(); len(f) != 0 {
			t.Errorf("%s: flashed %v", product, f)
		}
		for _, c := range fb.Calls() {
			if c == "reboot" {
				t.Errorf("%s: rebooted", product)
			}
		}
	}
}

func imageRelease(t *testing.T, prefix string) (*httptest.Server, []byte) {
	t.Helper()
	content := []byte("test firmware image")
	digest := sha256.Sum256(content)
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/img" {
			_, _ = w.Write(content)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"tag_name": "v5.3.0.2", "assets": []map[string]string{{"name": prefix + "-5.3.0.2-EngineX-OS-test-Update.img", "browser_download_url": srv.URL + "/img", "digest": "sha256:" + hex.EncodeToString(digest[:])}}})
	}))
	t.Cleanup(srv.Close)
	return srv, content
}

func TestGen1SSH(t *testing.T) {
	for _, tc := range []struct{ board, prefix string }{{"jc11", "PRIME4"}, {"jp07", "SC5000"}} {
		t.Run(tc.board, func(t *testing.T) {
			o := testOpts(t)
			o.defaults()
			srv, content := imageRelease(t, tc.prefix)
			useRelease(o, srv)
			d := startSSHDeck(t, o.SSHPort, "inmusic,"+tc.board+"\x00")
			if err := modeA(ctxT(t), o, deck{IP: "127.0.0.1"}); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(filepath.Join(d.Data, "az01-update.img"))
			if err != nil || !bytes.Equal(got, content) {
				t.Fatalf("image: %v", err)
			}
			if !strings.Contains(strings.Join(d.Commands(), "\n"), "az01-update --file") {
				t.Fatal("update command missing")
			}
		})
	}
}

func TestGen1Fastboot(t *testing.T) {
	for _, tc := range []struct{ board, prefix string }{{"JC11", "PRIME4"}, {"jp07", "SC5000"}} {
		t.Run(tc.board, func(t *testing.T) {
			o := testOpts(t)
			o.defaults()
			fb := newFastboot(t, tc.board, true)
			o.Fastboot = fb.Bin
			srv, _ := imageRelease(t, tc.prefix)
			useRelease(o, srv)
			o.Packer = filepath.Join(t.TempDir(), "packer.py")
			if err := os.WriteFile(o.Packer, []byte("import pathlib,sys\np=pathlib.Path(sys.argv[sys.argv.index('--output')+1]); p.mkdir(); (p/'rootfs.ext2').write_bytes(b'rootfs')\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := modeB(ctxT(t), o); err != nil {
				t.Fatal(err)
			}
			calls := strings.Join(fb.Calls(), "\n")
			for _, want := range []string{"oem inmusic-unlock-magic-7de5fbc22b8c524e", "flash rootfs ", "reboot"} {
				if !strings.Contains(calls, want) {
					t.Fatalf("missing %s: %s", want, calls)
				}
			}
			out := o.Out.(*bytes.Buffer).String()
			for _, want := range []string{
				"正在解锁碟机…",
				"Flashing rootfs partition (6 B) over USB; this may take several minutes. Do not disconnect USB or power off the deck...",
				"正在向碟机写入 rootfs 分区 (6 B)，通过 USB 传输可能需要几分钟，请勿断开连接或关闭电源…",
				"Sending 'rootfs'",
				"Writing 'rootfs'",
				"OKAY",
				"正在重启碟机…",
				"Rebooting deck...",
			} {
				if !strings.Contains(out, want) {
					t.Errorf("missing %q in Gen1 output:\n%s", want, out)
				}
			}
		})
	}
}

func TestModeBRefusesACorruptBundle(t *testing.T) {
	need(t, "python3", "mke2fs", "debugfs")
	b := corrupt(t, buildBundle(t, "PRIME4PLUS", "5.3.0.2"))
	fb := newFastboot(t, "JC11S", true)
	o := modeBOpts(t, fb, b)
	if err := run(ctxT(t), o); err == nil || !strings.Contains(err.Error(), "SHA256SUMS") {
		t.Fatalf("err = %v, want a SHA256SUMS refusal", err)
	}
	if f := fb.Flashed(); len(f) != 0 {
		t.Errorf("flashed %v", f)
	}
}

func TestModeAInstallsNothingFromACorruptBundle(t *testing.T) {
	need(t, "python3", "unshare")
	b := corrupt(t, buildBundle(t, "PRIME4PLUS", "5.3.0.2"))
	o := testOpts(t)
	useRelease(o, releaseServer(t, b, "", nil))
	deck := startSSHDeck(t, o.SSHPort, "inmusic,jc11s\x00")
	announceFrom(t, "127.0.0.1", freeUDPFrom(o), airD("lab", "DISCOVERER_HOWDY_", "JC11S", "2", 1))
	if err := run(ctxT(t), o); err == nil || !strings.Contains(err.Error(), "SHA256SUMS") {
		t.Fatalf("err = %v, want a SHA256SUMS refusal", err)
	}
	if !strings.Contains(strings.Join(deck.Commands(), "\n"), "compatible") {
		t.Error("never got as far as asking the deck which model it is")
	}
	for _, c := range deck.Commands() {
		if strings.Contains(c, "tar -xzf") {
			t.Errorf("sent the bundle to the deck: %s", c)
		}
	}
	if entries, _ := os.ReadDir(deck.Data); len(entries) != 0 {
		t.Errorf("deck data touched: %v", entries)
	}
}

func TestDownloadRefusesAWrongReleaseDigest(t *testing.T) {
	need(t, "python3")
	b := buildBundle(t, "PRIME4PLUS", "5.3.0.2")
	fb := newFastboot(t, "JC11S", true)
	o := testOpts(t)
	o.Fastboot = fb.Bin
	useRelease(o, releaseServer(t, b, "sha256:"+strings.Repeat("ab", 32), nil))
	if err := run(ctxT(t), o); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("err = %v, want a SHA-256 mismatch", err)
	}
	if f := fb.Flashed(); len(f) != 0 {
		t.Errorf("flashed %v", f)
	}
}

func TestModeBAsksBeforeFlashing(t *testing.T) {
	need(t, "python3", "mke2fs", "debugfs", "e2fsck")
	b := buildBundle(t, "PRIME4PLUS", "5.3.0.2")
	fb := newFastboot(t, "JC11S", true)
	o := modeBOpts(t, fb, b)
	o.Yes, o.In = false, strings.NewReader("n\n")
	if err := run(ctxT(t), o); err == nil || !strings.Contains(err.Error(), "取消") {
		t.Fatalf("err = %v, want a cancel", err)
	}
	if img, _ := filepath.Glob(filepath.Join(o.WorkDir, "*.img")); len(img) == 0 {
		t.Error("should have built the image and asked, not stopped earlier")
	}
	if f := fb.Flashed(); len(f) != 0 {
		t.Errorf("flashed %v", f)
	}
}

func TestLockedDeckWaitsForUSBThenFlashes(t *testing.T) {
	need(t, "python3", "mke2fs", "debugfs", "e2fsck")
	b := buildBundle(t, "PRIME4PLUS", "5.3.0.2")
	fb := newFastboot(t, "JC11S", true)
	// The deck is announced but SSH is closed, and fastboot reports no device
	// until the user has rebooted it: the shim plays that by hiding devices at first.
	hidden := filepath.Join(fb.Dir, "devices.real")
	os.Rename(fb.Bin, hidden)
	os.WriteFile(fb.Bin, []byte(fmt.Sprintf(`#!/bin/sh
d=$(dirname "$0")
if [ "$1" = devices ] && [ ! -f "$d/usb" ]; then echo "$*" >> "$d/calls.log"; touch "$d/polled"; [ -f "$d/polled2" ] && touch "$d/usb"; touch "$d/polled2"; exit 0; fi
exec sh %q "$@"
`, hidden)), 0o755)
	o := modeBOpts(t, fb, b)
	o.WaitUSB = 20 * time.Second
	announceFrom(t, "127.0.0.1", freeUDPFrom(o), airD("prime4plus-lab", "DISCOVERER_HOWDY_", "JC11S", "2", 1))
	if err := run(ctxT(t), o); err != nil {
		t.Fatalf("run: %v\n%s", err, o.Out)
	}
	if f := fb.Flashed(); len(f) != 1 {
		t.Fatalf("flash calls = %v", f)
	}
	if !strings.Contains(o.Out.(*bytes.Buffer).String(), "prime4plus-lab (127.0.0.1)") {
		t.Errorf("the user was not told which deck was found:\n%s", o.Out)
	}
}

// The shell installer picks the same partition as the Go one.
func TestFastbootScriptPicksDataOrUserdata(t *testing.T) {
	for _, hasData := range []bool{true, false} {
		fb := newFastboot(t, "JC11S", hasData)
		img := filepath.Join(t.TempDir(), "u.img")
		os.WriteFile(img, []byte("image"), 0o644)
		cmd := exec.Command("sh", filepath.Join(repoRoot(t), "tools/az01-packer/host-install-fastboot.sh"), img)
		cmd.Env = append(os.Environ(), "PATH="+fb.Dir+":"+os.Getenv("PATH"))
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("script: %v\n%s", err, out)
		}
		want := "flash userdata "
		if hasData {
			want = "flash data "
		}
		if f := fb.Flashed(); len(f) != 1 || !strings.HasPrefix(f[0], want) {
			t.Errorf("hasData=%v: flash calls = %v, want %q", hasData, f, want)
		}
	}
}

// #104: a 1st-gen deck in normal operation is invisible to the tool; say how to flash it.
func TestNoDeckPrintsFirstGenGuide(t *testing.T) {
	o := testOpts(t)
	err := run(ctxT(t), o)
	if err == nil || !strings.Contains(err.Error(), "没有发现碟机") {
		t.Fatalf("err = %v, want the no-deck error", err)
	}
	out := o.Out.(*bytes.Buffer).String()
	for _, want := range []string{"PRIME GO", "Settings -> Update -> Reboot", "Reboot to Computer Update Mode", "15a2:007d", "FAT32/exFAT", "SYSTEM ONE", "51337"} {
		if !strings.Contains(out, want) {
			t.Errorf("guide is missing %q", want)
		}
	}
	if strings.Count(out, "方式 B（USB 线）") != 2 {
		t.Error("expected the guide both at start-up and when no deck is found")
	}
}

// #116: detect Rockchip USB loaders: 2207:350b (RK3588 RMZ2/JP24) and 2207:320a (RK3288).
func TestIsRockchipUSBLoader(t *testing.T) {
	tests := []struct {
		line string
		want bool
	}{
		{"Bus 001 Device 004: ID 2207:350b Fuzhou Rockchip Electronics Company", true},
		{"2207:350b", true},
		{"2207:350B", true},
		{"ID 2207:320a", true},
		{"2207:320A", true},
		{"Bus 002 Device 005: ID 2207:320a Rockchip RK3288 Loader", true},
		{"Bus 001 Device 003: ID 15a2:007d Freescale Semiconductor, Inc. i.MX6", false},
		{"ID 2207:0011 Rockchip ADB", false},
		{"", false},
		{"some random text without rockchip vendor id", false},
	}

	for _, tt := range tests {
		got := IsRockchipUSBLoader(tt.line)
		if got != tt.want {
			t.Errorf("IsRockchipUSBLoader(%q) = %v, want %v", tt.line, got, tt.want)
		}
	}
}

// #129: PRIME 4+ shows up under several product spellings.
func TestBoardCodeJC11SVariants(t *testing.T) {
	for _, s := range []string{"inmusic,jc11s", "rk3288-az0x-jc11s", "prime4plus", "PRIME4+", "PRIME 4+", "JC11S", " jc11s\n"} {
		if got := boardCode(s); got != "jc11s" {
			t.Errorf("boardCode(%q) = %q, want jc11s", s, got)
		}
		if m, gen2, ok := lookupBoard(s); !ok || !gen2 || m.Prefix != "PRIME4PLUS" {
			t.Errorf("lookupBoard(%q) = %v %v %v", s, m, gen2, ok)
		}
	}
	if got := boardCode("jc11"); got != "jc11" {
		t.Errorf("boardCode(jc11) = %q: PRIME 4 must stay first-gen", got)
	}
}

func logShim(t *testing.T, body string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "fastboot")
	os.WriteFile(bin, []byte("#!/bin/sh\necho \"$*\" >> \""+dir+"/calls.log\"\n"+body), 0o755)
	return bin, filepath.Join(dir, "calls.log")
}

// #129: old fastboot builds only list whitelisted vendors; try inMusic 0x15e4, then Rockchip 0x2207, and keep what worked.
func TestFastbootFallsBackToRockchipVID(t *testing.T) {
	bin, log := logShim(t, `[ "$1 $2 $3" = "-i 0x2207 devices" ] && printf 'RK0001\tfastboot\n'
exit 0
`)
	o := testOpts(t)
	o.Fastboot = bin
	o.defaults()
	if s := fastbootDevices(ctxT(t), o); len(s) != 1 || s[0] != "RK0001" {
		t.Fatalf("serials = %v", s)
	}
	fb(ctxT(t), o, "getvar", "product")
	b, _ := os.ReadFile(log)
	if got := strings.TrimSpace(string(b)); got != "devices\n-i 0x15e4 devices\n-i 0x2207 devices\n-i 0x2207 getvar product" {
		t.Errorf("calls:\n%s", got)
	}
}

func TestFastbootWithoutDeviceDoesNotKeepVID(t *testing.T) {
	bin, _ := logShim(t, "exit 0\n")
	o := testOpts(t)
	o.Fastboot = bin
	o.defaults()
	if s := fastbootDevices(ctxT(t), o); len(s) != 0 || o.fbVID != "" {
		t.Errorf("serials = %v, fbVID = %q", s, o.fbVID)
	}
}

// #129: RattyDAVE's Arch android-tools prints "Android Fastboot" (capital F, two words), with or without -l,
// and rejects -i; the deck must be found and -i must not be retried.
func TestFastbootListsAndroidFastboot(t *testing.T) {
	for _, line := range []string{`A12303258600613\tAndroid Fastboot`, `A12303258600613        Android Fastboot usb:3-3.1`, `A12303258600613\tfastboot`} {
		bin, _ := logShim(t, `[ "$1" = devices ] && printf '`+line+`\n'
exit 0
`)
		o := testOpts(t)
		o.Fastboot = bin
		o.defaults()
		if s := fastbootDevices(ctxT(t), o); len(s) != 1 || s[0] != "A12303258600613" || o.fbVID != "" {
			t.Errorf("%q: serials = %v, fbVID = %q", line, s, o.fbVID)
		}
	}
	bin, log := logShim(t, `[ "$1" = -i ] && { echo "fastboot: invalid option -- 'i'" >&2; exit 1; }
exit 0
`)
	o := testOpts(t)
	o.Fastboot = bin
	o.defaults()
	if s := fastbootDevices(ctxT(t), o); len(s) != 0 || o.fbVID != "" {
		t.Errorf("serials = %v, fbVID = %q", s, o.fbVID)
	}
	if b, _ := os.ReadFile(log); strings.TrimSpace(string(b)) != "devices\n-i 0x15e4 devices" {
		t.Errorf("calls:\n%s", b)
	}
}

// #129: Linux fastboot without udev access lists "no permissions"; tell the user how to fix it, once.
func TestFastbootNoPermissionsHint(t *testing.T) {
	bin, _ := logShim(t, `printf '????????????\tno permissions (missing udev rules? user is in the plugdev group); see [http://developer.android.com/tools/device.html]\n'
`)
	t.Cleanup(func() { geteuid = os.Geteuid })
	for _, uid := range []int{1000, 0} {
		geteuid = func() int { return uid }
		o := testOpts(t)
		o.Fastboot = bin
		o.defaults()
		fastbootDevices(ctxT(t), o)
		if s := fastbootDevices(ctxT(t), o); len(s) != 0 {
			t.Errorf("serials = %v", s)
		}
		out := o.Out.(*bytes.Buffer).String()
		if uid == 0 && strings.Contains(out, "sudo") { // already root: "use sudo" would be a lie
			t.Errorf("root was told to use sudo:\n%s", out)
		}
		if uid != 0 && (strings.Count(out, "/etc/udev/rules.d/99-enginex.rules") != 2 || !strings.Contains(out, " 15e4 1cc5 ")) { // zh + en, printed once
			t.Errorf("hint:\n%s", out)
		}
	}
}

// #129: RattyDAVE ran as root with a normally booted PRIME 4+ on USB (15e4:d007) and no fastboot installed.
func TestUSBDeckWithoutFastbootSaysInstallFastboot(t *testing.T) {
	t.Cleanup(func() { geteuid = os.Geteuid })
	geteuid = func() int { return 0 }
	o := testOpts(t)
	o.ListUSB = func(context.Context) string {
		return "15e4:e008 Denon DJ PRIME 4 Plus Hub\n15e4:d007 DENON DJ Denon DJ PRIME 4 Plus\n"
	}
	err := run(ctxT(t), o)
	if err == nil || !strings.Contains(err.Error(), "fastboot is not installed") || !strings.Contains(err.Error(), "15e4:d007") {
		t.Fatalf("err = %v", err)
	}
	if out := o.Out.(*bytes.Buffer).String() + err.Error(); strings.Contains(out, "with sudo") || strings.Contains(out, "用 sudo") {
		t.Errorf("told to use sudo:\n%s", out)
	}
}

// #129: a normally booted PRIME 4+ on USB, not on the LAN: wait for Firmware Update Mode, then flash.
func TestUSBDeckWaitsForFirmwareUpdateMode(t *testing.T) {
	need(t, "python3", "mke2fs", "debugfs", "e2fsck")
	b := buildBundle(t, "PRIME4PLUS", "5.3.0.2")
	fb := newFastboot(t, "JC11S", true)
	hidden := filepath.Join(fb.Dir, "devices.real")
	os.Rename(fb.Bin, hidden)
	os.WriteFile(fb.Bin, []byte(fmt.Sprintf(`#!/bin/sh
d=$(dirname "$0")
if [ "$1" = devices ] && [ ! -f "$d/usb" ]; then n=$(cat "$d/n" 2>/dev/null || echo 0); echo $((n+1)) > "$d/n"; [ "$n" -ge 3 ] && touch "$d/usb"; exit 0; fi
exec sh %q "$@"
`, hidden)), 0o755)
	o := modeBOpts(t, fb, b)
	o.WaitUSB = 20 * time.Second
	o.ListUSB = func(context.Context) string { return "15e4:d007 DENON DJ Denon DJ PRIME 4 Plus\n" }
	if err := run(ctxT(t), o); err != nil {
		t.Fatalf("run: %v\n%s", err, o.Out)
	}
	if f := fb.Flashed(); len(f) != 1 {
		t.Fatalf("flash calls = %v", f)
	}
	if out := o.Out.(*bytes.Buffer).String(); !strings.Contains(out, "Denon DJ PRIME 4+") || !strings.Contains(out, "Firmware Update Mode") {
		t.Errorf("the user was not told what to do:\n%s", out)
	}
}

// #129: the deck is on the USB bus but fastboot sees nothing: say so, with its id.
func TestDiagnoseUSB(t *testing.T) {
	o := testOpts(t)
	o.ListUSB = func(context.Context) string { return "1d6b:0002 Linux Foundation\n2207:320a Rockchip USB-MSC\n" }
	o.defaults()
	diagnoseUSB(ctxT(t), o)
	diagnoseUSB(ctxT(t), o)
	if out := o.Out.(*bytes.Buffer).String(); strings.Count(out, "2207:320a Rockchip USB-MSC") != 2 || strings.Contains(out, "1d6b") {
		t.Errorf("diagnosis:\n%s", out)
	}
	o = testOpts(t)
	o.ListUSB = func(context.Context) string { return "1d6b:0002 Linux Foundation\n" }
	diagnoseUSB(ctxT(t), o)
	if out := o.Out.(*bytes.Buffer).String(); out != "" {
		t.Errorf("no deck on USB, yet:\n%s", out)
	}
}

func TestIsDeckUSB(t *testing.T) {
	for line, want := range map[string]bool{
		"2207:320a": true, "2207:0006 Android": true, "18d1:4ee0 Google fastboot": true,
		"0e8d:1234 inMusic Brands PRIME 4+": true, "1234:5678 Denon PRIME 4": true, "15e4:d012 DENON DJ SCX-4": true,
		"15a2:007d Freescale": false, "2207:0011 Rockchip ADB": false, "": false,
	} {
		if got := isDeckUSB(line); got != want {
			t.Errorf("isDeckUSB(%q) = %v", line, got)
		}
	}
	win := winVIDPID.ReplaceAllString(`Instance ID:   USB\VID_2207&PID_0006\0123456789`, "$1:$2")
	if !isDeckUSB(win) {
		t.Errorf("pnputil line not recognised: %s", win)
	}
	mac := parseIOReg(`+-o Root  <class IORegistryEntry>
  +-o PRIME 4+@14100000  <class IOUSBHostDevice>
    {
      "idProduct" = 12810
      "idVendor" = 8711
    }
  +-o Keyboard@14200000  <class IOUSBHostDevice>
    {
      "idVendor" = 1452
      "idProduct" = 641
    }
`)
	if mac != "2207:320a PRIME 4+\n05ac:0281 Keyboard\n" {
		t.Errorf("parseIOReg = %q", mac)
	}
}

type failWriter struct{ err error }

func (f failWriter) Write(p []byte) (int, error) { return 0, f.err }

type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func TestCaptureWriterSwallowsDisplayErrors(t *testing.T) {
	cw := &captureWriter{
		buf: &bytes.Buffer{},
		out: failWriter{err: errors.New("broken pipe")},
	}
	msg := []byte("Sending 'userdata'...\n")
	n, err := cw.Write(msg)
	if err != nil || n != len(msg) {
		t.Fatalf("Write returned (%d, %v), want (%d, nil)", n, err, len(msg))
	}
	if cw.buf.String() != string(msg) {
		t.Fatalf("buffer = %q, want %q", cw.buf.String(), string(msg))
	}
}

// #129: writing to a broken stdout pipe (fd 1) during flash must not terminate the installer with SIGPIPE.
func TestSigpipeStdoutSubprocess(t *testing.T) {
	if os.Getenv("TEST_SIGPIPE_CHILD") == "1" {
		fb := newFastboot(t, "JC11S", false)
		b := buildBundle(t, "PRIME4PLUS", "5.3.0.2")
		o := modeBOpts(t, fb, b)
		o.Out = os.Stdout
		if err := run(ctxT(t), o); err != nil {
			t.Fatalf("run failed: %v", err)
		}
		if f := fb.Flashed(); len(f) != 1 || !strings.Contains(f[0], "flash userdata ") {
			t.Fatalf("unexpected flash calls: %v", f)
		}
		return
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	r.Close() // close read end so descriptor 1 encounters broken pipe (SIGPIPE)

	cmd := exec.Command(os.Args[0], "-test.run=^TestSigpipeStdoutSubprocess$")
	cmd.Env = append(os.Environ(), "TEST_SIGPIPE_CHILD=1")
	cmd.Stdout = w
	err = cmd.Run()
	w.Close()
	if err != nil {
		t.Fatalf("subprocess died or failed with broken pipe on fd 1: %v", err)
	}
}

func TestFormatBytes(t *testing.T) {
	for bytes, want := range map[int64]string{
		500:                "500 B",
		1536:               "1.5 KB",
		1024 * 500:         "500.0 KB",
		1024 * 1024 * 12:   "12.0 MB",
		1024 * 1024 * 1024: "1.0 GB",
		15032385536:        "14.0 GB",
	} {
		if got := formatBytes(bytes); got != want {
			t.Errorf("formatBytes(%d) = %q, want %q", bytes, got, want)
		}
	}
}

// #129: RattyDAVE reported a long pause after typing YES; the installer must stream fastboot's
// live transfer progress to o.Out and display file size and reassurance before flashing.
func TestModeBStreamsFlashProgressAndSize(t *testing.T) {
	need(t, "python3", "mke2fs", "debugfs", "e2fsck")
	b := buildBundle(t, "PRIME4PLUS", "5.3.0.2")
	fb := newFastboot(t, "JC11S", false)
	o := modeBOpts(t, fb, b)
	if err := run(ctxT(t), o); err != nil {
		t.Fatalf("run: %v\n%s", err, o.Out)
	}
	out := o.Out.(*bytes.Buffer).String()
	for _, want := range []string{
		"Flashing userdata partition (64.0 MB) over USB; this may take several minutes. Do not disconnect USB or power off the deck...",
		"正在向碟机写入 userdata 分区 (64.0 MB)，通过 USB 传输可能需要几分钟，请勿断开连接或关闭电源…",
		"Sending 'userdata'",
		"Writing 'userdata'",
		"OKAY",
		"正在重启碟机…",
		"Rebooting deck...",
		"Done: EngineX OS",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("installer output missing %q:\n%s", want, out)
		}
	}
}

// #129: verify fbLive delivers stdout/stderr chunks before the process finishes.
func TestFbLiveStreamsBeforeProcessExit(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "slow-fastboot")
	syncFile := filepath.Join(dir, "sync")
	script := fmt.Sprintf(`#!/bin/sh
echo "line 1: sending" >&2
touch %q
for i in $(seq 1 100); do
  if [ -f %q.ack ]; then break; fi
  sleep 0.05
done
echo "line 2: done" >&2
`, syncFile, syncFile)
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	outBuf := &safeBuffer{}
	o := testOpts(t)
	o.Fastboot = bin
	o.Out = outBuf
	o.defaults()

	done := make(chan error, 1)
	go func() {
		_, err := fbLive(ctxT(t), o, "flash", "userdata", "dummy.img")
		done <- err
	}()

	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(outBuf.String(), "line 1: sending") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(outBuf.String(), "line 1: sending") {
		t.Fatalf("timed out waiting for early streaming output, got %q", outBuf.String())
	}

	// Verify child process is still actively waiting and has not exited
	select {
	case err := <-done:
		t.Fatalf("child process exited prematurely before ack: %v", err)
	default:
	}

	if err := os.WriteFile(syncFile+".ack", []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("fbLive failed: %v", err)
	}

	if !strings.Contains(outBuf.String(), "line 2: done") {
		t.Fatalf("expected completion output, got %q", outBuf.String())
	}
}

// ---- mode C: 1cc5:2027 Firmware Update Mode (#116) --------------------------

func TestAZModel(t *testing.T) {
	for _, c := range []struct{ board, bundle, line, want string }{
		{"RMZ2", "", "1cc5:2027", "SYSTEMONE"},
		{"", "/x/PRIME4G2-5.3.0.3-EngineX-OS-Data-Bundle.tar.gz", "1cc5:2027", "PRIME4G2"},
		{"", "", "1cc5:2027 inMusic Rane SYSTEM ONE", "SYSTEMONE"},
		{"", "", "1cc5:2027 Denon DJ PRIME 4 G2", "PRIME4G2"},
		{"", "", "1cc5:2027 Denon DJ PRIME 4+", "PRIME4PLUS"},
		{"", "", "1cc5:2027 Numark Mixstream Pro Go", "MIXSTREAMPROGO"},
		{"", "", "1cc5:2027 Numark Mixstream Pro+", "MIXSTREAMPROPLUS"},
		{"jp07", "", "1cc5:2027", ""},
		{"", "", "1cc5:2027 az0x usbupdate", ""},
	} {
		m, ok := azModel(&options{Bundle: c.bundle}, c.board, c.line)
		if ok != (c.want != "") || m.Prefix != c.want {
			t.Errorf("azModel(%q, %q, %q) = %v %v, want %q", c.board, c.bundle, c.line, m, ok, c.want)
		}
	}
}

func TestAZModelLocalBundle(t *testing.T) {
	dir := t.TempDir()
	o := &options{WorkDir: dir}
	if _, ok := azModel(o, "", "1cc5:2027"); ok {
		t.Fatal("empty work dir must not identify a model")
	}
	one := filepath.Join(dir, "PRIME4G2-5.3.0.3-EngineX-OS-Data-Bundle.tar.gz")
	if err := os.WriteFile(one, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if m, ok := azModel(o, "", "1cc5:2027"); !ok || m.Prefix != "PRIME4G2" {
		t.Fatalf("one local bundle: got %v %v", m, ok)
	}
	if err := os.WriteFile(filepath.Join(dir, "SYSTEMONE-5.3.0.3-EngineX-OS-Data-Bundle.tar.gz"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := azModel(o, "", "1cc5:2027"); ok {
		t.Fatal("two local bundles are ambiguous")
	}
}

type usbShim struct {
	script []byte
	img    string
	calls  int
}

func azOpts(t *testing.T, b bundleFile, usb func(int) string) (*options, *usbShim) {
	o := testOpts(t)
	useRelease(o, releaseServer(t, b, "", nil))
	n := 0
	o.ListUSB = func(context.Context) string { n++; return usb(n) }
	shim := &usbShim{}
	o.USBFlash = func(_ context.Context, o *options, script []byte, img string) error {
		shim.calls++
		shim.script = script
		shim.img = filepath.Join(t.TempDir(), "flashed.img")
		fmt.Fprintln(o.Out, "writing data: 100% (64 / 64 MiB)")
		return copyFile(img, shim.img)
	}
	return o, shim
}

func TestUSBUpdateModeFlashesA64MiBDataImage(t *testing.T) {
	need(t, "python3", "mke2fs", "debugfs", "e2fsck", "dumpe2fs")
	b := buildBundle(t, "SYSTEMONE", "5.3.0.3")
	o, shim := azOpts(t, b, func(int) string { return "1d6b:0002 Linux Foundation\n1cc5:2027 inMusic SYSTEM ONE\n" })
	if err := run(ctxT(t), o); err != nil {
		t.Fatalf("run: %v\n%s", err, o.Out)
	}
	if shim.calls != 1 {
		t.Fatalf("USB flash calls = %d", shim.calls)
	}
	want, _ := os.ReadFile(filepath.Join(repoRoot(t), "tools/az01-packer/host-install-usbupdate.py"))
	if !bytes.Equal(shim.script, want) {
		t.Error("flasher is not the bundle's host-install-usbupdate.py")
	}
	if fi, _ := os.Stat(shim.img); fi == nil || fi.Size() != 64<<20 {
		t.Fatalf("image size = %v, want 64 MiB", fi)
	}
	sh(t, "e2fsck", "-fn", shim.img)
	hdr := sh(t, "dumpe2fs", "-h", shim.img)
	for _, w := range []string{"Filesystem volume name:   data\n", "encrypt", "Block size:               4096"} {
		if !strings.Contains(hdr, w) {
			t.Errorf("superblock lacks %q:\n%s", w, hdr)
		}
	}
	out := filepath.Join(t.TempDir(), "suite")
	sh(t, "debugfs", "-R", "dump /enginex/bundle/bin/sc5000-suite "+out, shim.img)
	if sha(t, out) != sha(t, b.SuitePath) {
		t.Error("suite in the image differs from the bundle's")
	}
	if s := sh(t, "debugfs", "-R", "stat /system/var-lib/alsa/conf.d/99-sc5000-custom.conf", shim.img); !strings.Contains(s, "Type: regular") {
		t.Errorf("AZ04 ALSA hook missing:\n%s", s)
	}
	if s := o.Out.(*bytes.Buffer).String(); !strings.Contains(s, "Rane SYSTEM ONE") || !strings.Contains(s, "100%") || !strings.Contains(s, "power button") {
		t.Errorf("output:\n%s", s)
	}
}

// A SYSTEM ONE seen on the LAN, no fastboot installed: wait until it shows up as 1cc5:2027, then flash.
func TestLockedSystemOneWaitsFor1cc5(t *testing.T) {
	need(t, "python3", "mke2fs", "debugfs", "e2fsck")
	b := buildBundle(t, "SYSTEMONE", "5.3.0.3")
	o, shim := azOpts(t, b, func(n int) string {
		if n < 3 {
			return ""
		}
		return "1cc5:2027 \n" // no product name: the model comes from the LAN
	})
	o.WaitUSB = 20 * time.Second
	o.defaults()
	if err := modeAB(ctxT(t), o, deck{Name: "SYSTEM ONE", IP: "127.0.0.1", Board: "RMZ2"}); err != nil {
		t.Fatalf("modeAB: %v\n%s", err, o.Out)
	}
	if shim.calls != 1 {
		t.Fatalf("USB flash calls = %d", shim.calls)
	}
}

func TestUSBUpdateModeRefusals(t *testing.T) {
	need(t, "python3", "mke2fs", "debugfs", "e2fsck")
	b := buildBundle(t, "SYSTEMONE", "5.3.0.3")
	o, shim := azOpts(t, b, func(int) string { return "1cc5:2027 usbupdate\n" })
	if err := run(ctxT(t), o); err == nil || !strings.Contains(err.Error(), "--bundle") {
		t.Errorf("unknown model: err = %v", err)
	}
	o, shim2 := azOpts(t, b, func(int) string { return "1cc5:2027 SYSTEM ONE\n" })
	o.Yes, o.In = false, strings.NewReader("n\n")
	if err := run(ctxT(t), o); err == nil || !strings.Contains(err.Error(), "取消") {
		t.Errorf("declined: err = %v", err)
	}
	o, shim3 := azOpts(t, b, func(int) string { return "1cc5:2027 SYSTEM ONE\n" })
	o.USBFlash = func(context.Context, *options, []byte, string) error { return errors.New("status 2") }
	if err := run(ctxT(t), o); err == nil || !strings.Contains(err.Error(), "write-part: status 2") {
		t.Errorf("flash failure: err = %v", err)
	}
	if shim.calls+shim2.calls+shim3.calls != 0 {
		t.Error("flashed anyway")
	}
}

// The real flasher runs from the bundle under python3 -E and reports why it cannot write.
func TestUSBFlashRunsTheBundleScript(t *testing.T) {
	if python() == "" {
		t.Skip("python3 not installed")
	}
	script, _ := os.ReadFile(filepath.Join(repoRoot(t), "tools/az01-packer/host-install-usbupdate.py"))
	img := filepath.Join(t.TempDir(), "data.img")
	os.WriteFile(img, make([]byte, 4096), 0o644)
	o := testOpts(t)
	if err := usbFlash(ctxT(t), o, script, img); err == nil {
		t.Fatal("no deck on this host, yet the flash succeeded")
	}
	if s := o.Out.(*bytes.Buffer).String(); !strings.Contains(s, "host-install-usbupdate: ") {
		t.Errorf("output:\n%s", s)
	}
}
