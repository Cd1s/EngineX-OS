// Command enginex-installer installs EngineX OS on a supported deck
// with no input: it finds the deck, works out the model and picks the bundle (#88).
//
//	mode A: the deck is on the LAN with SSH open -> stream the Data-Bundle over SSH.
//	mode B: the deck is in fastboot (USB)         -> build a /data image and flash it.
//	mode C: Firmware Update Mode as 1cc5:2027      -> build a 64 MiB /data image, write-part it over USB (#116).
package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"golang.org/x/crypto/ssh"
)

const (
	bundleSuffix    = "-EngineX-OS-Data-Bundle.tar.gz"
	bundleMaxDown   = 64 << 20  // compressed, as the deck's own updater allows
	bundleMaxUnpack = 256 << 20 // all files together
	gen2ImgPrefix   = "ENGINEX_DATA_BUNDLE"
	gen2ImgMagic    = "ENGINEX_DATA_BUNDLE_IMG_V1\n"
	gen2ImgHeader   = 128
	compatCmd       = "cat /sys/firmware/devicetree/base/compatible"
	// install.sh's own default; the variable is only set by tests.
	customDirSh = `"${SC5000_CUSTOM:-/data/enginex}"`
)

func init() {
	// On macOS (Darwin), Homebrew installs e2fsprogs as keg-only to
	// /opt/homebrew/opt/e2fsprogs/sbin or /usr/local/opt/e2fsprogs/sbin.
	// Ensure common tool paths are in PATH so mke2fs and debugfs are discovered.
	if runtime.GOOS != "darwin" {
		return
	}
	extra := []string{
		"/opt/homebrew/opt/e2fsprogs/sbin", "/opt/homebrew/opt/e2fsprogs/bin",
		"/opt/homebrew/bin", "/opt/homebrew/sbin",
		"/usr/local/opt/e2fsprogs/sbin", "/usr/local/opt/e2fsprogs/bin",
		"/usr/local/bin", "/usr/local/sbin",
	}
	path := os.Getenv("PATH")
	existing := map[string]bool{}
	for _, p := range filepath.SplitList(path) {
		existing[p] = true
	}
	var toPrepend []string
	for _, dir := range extra {
		if _, err := os.Stat(dir); err == nil && !existing[dir] {
			toPrepend = append(toPrepend, dir)
			existing[dir] = true
		}
	}
	if len(toPrepend) > 0 {
		os.Setenv("PATH", strings.Join(toPrepend, string(os.PathListSeparator))+string(os.PathListSeparator)+path)
	}
}

// options are the flags plus the seams the tests point at real local things.
type options struct {
	Bundle, Backup, Key, Password string
	Repo, API                     string
	Yes                           bool
	Uninstall                     bool
	HTTP                          *http.Client
	Fastboot                      string // fastboot binary
	Packer                        string
	AirDAddr, EAASAddr            string        // StageLinQ and EAAS UDP listen addresses
	AirDWait                      time.Duration // how long to listen
	Subnets                       []string      // "192.168.1" prefixes to scan; nil = this machine's /24s
	UIPort, SSHPort               int
	WorkDir                       string
	WaitUSB                       time.Duration
	Out                           io.Writer
	In                            io.Reader
	ListUSB                       func(context.Context) string // host USB devices, one "vid:pid name" per line
	fbVID                         string                       // "-i" vendor id fastboot needs to see the deck
	hinted                        map[string]bool
	// USBFlash writes img in mode C (1cc5:2027) with the bundle's host-install-usbupdate.py.
	USBFlash func(ctx context.Context, o *options, script []byte, img string) error
}

func (o *options) defaults() {
	if o.Repo == "" {
		o.Repo = "Cd1s/EngineX-OS"
	}
	if o.API == "" {
		o.API = "https://api.github.com"
	}
	if o.HTTP == nil {
		o.HTTP = &http.Client{Timeout: 10 * time.Minute}
	}
	if o.Fastboot == "" {
		o.Fastboot = findFastboot()
	}
	if o.AirDAddr == "" {
		o.AirDAddr = ":51337"
	}
	if o.EAASAddr == "" {
		o.EAASAddr = ":11224"
	}
	if o.AirDWait == 0 {
		o.AirDWait = 3500 * time.Millisecond
	}
	if o.UIPort == 0 {
		o.UIPort = 8080
	}
	if o.SSHPort == 0 {
		o.SSHPort = 22
	}
	if o.WorkDir == "" {
		o.WorkDir = "."
	}
	if o.WaitUSB == 0 {
		o.WaitUSB = 10 * time.Minute
	}
	if o.Out == nil {
		o.Out = os.Stdout
	}
	if o.In == nil {
		o.In = os.Stdin
	}
	if o.ListUSB == nil {
		o.ListUSB = hostUSB
	}
	if o.USBFlash == nil {
		o.USBFlash = usbFlash
	}
}

// hint prints a message once per run (the USB wait loop polls every second).
func (o *options) hint(key, zh, en string) {
	if o.hinted[key] {
		return
	}
	if o.hinted == nil {
		o.hinted = map[string]bool{}
	}
	o.hinted[key] = true
	o.say(zh, en)
}

var sayMu sync.Mutex

func (o *options) say(zh, en string) {
	sayMu.Lock()
	defer sayMu.Unlock()
	fmt.Fprintf(o.Out, "%s\n  %s\n", zh, en)
}

// firstGenGuide is shown at start-up and when no deck is found: the first-generation decks
// are invisible to this tool in normal operation (#104, "flasher doesn't see Prime Go").
const firstGenGuide = `==================================================================
 [一代机 / 1st-gen: SC5000 SC5000M SC6000 SC6000M PRIME GO PRIME 4 PRIME 2 Mixstream Pro]
  本工具在碟机正常开机时看不到一代机 / this tool cannot see a 1st-gen deck while it runs normally.
  方式 A（最简单，无需电脑）/ Option A (easiest, no computer):
    把对应机型的 *-Update.img 拷到 FAT32/exFAT U 盘根目录，插上碟机，
    点 Settings -> Update -> Reboot 即可刷入或恢复。
    Copy the model's *-Update.img to the root of a FAT32/exFAT USB stick, plug it into the
    deck, then tap Settings -> Update -> Reboot.
  方式 B（USB 线）/ Option B (USB cable):
    先在碟机上点 Settings -> Update -> Reboot to Computer Update Mode
    （进入电脑更新模式，USB ID 15a2:007d），再接 USB-B 线运行本工具。
    First tap Settings -> Update -> Reboot to Computer Update Mode on the deck
    (USB ID 15a2:007d), then connect the USB-B cable and run this tool.
 [二代机 / 2nd-gen: SC LIVE 2/4, Mixstream Pro+/Go, PRIME 4+, RANE PERFORMER, SYSTEM ONE]
  碟机正常开机，并与电脑在同一 Wi-Fi / 局域网，本工具会自动发现并一键安装。
  Keep the deck powered on and on the same Wi-Fi / LAN as this computer; it is found and installed automatically.
==================================================================`

func (o *options) guide() { fmt.Fprintln(o.Out, firstGenGuide) }

// ---- boards -----------------------------------------------------------------

type model struct{ Prefix, Name string }

// The key is the board code: fastboot's "product" and the device tree's "inmusic,<code>".
var gen2Boards = map[string]model{
	"jp21": {"SCLIVE4", "Denon DJ SC LIVE 4"}, "jp20": {"SCLIVE2", "Denon DJ SC LIVE 2"},
	"jc11s": {"PRIME4PLUS", "Denon DJ PRIME 4+"}, "jp11s": {"PRIMEGOPLUS", "Denon DJ PRIME GO+"},
	"nh08s": {"MIXSTREAMPROPLUS", "Numark Mixstream Pro+"}, "nh10": {"MIXSTREAMPROGO", "Numark Mixstream Pro Go"},
	"jp24": {"PRIME4G2", "Denon DJ PRIME 4 G2"}, "rmz2": {"SYSTEMONE", "Rane SYSTEM ONE"},
}

var gen1Boards = map[string]model{
	"jp07": {"SC5000", "Denon DJ SC5000"}, "jp08": {"SC5000M", "Denon DJ SC5000M"}, "jp11": {"PRIMEGO", "Denon DJ PRIME GO"},
	"jp13": {"SC6000", "Denon DJ SC6000"}, "jp14": {"SC6000M", "Denon DJ SC6000M"},
	"jc11": {"PRIME4", "Denon DJ PRIME 4"}, "jc16": {"PRIME2", "Denon DJ PRIME 2"}, "nh08": {"MIXSTREAMPRO", "Numark Mixstream Pro"},
}

// boardAliases maps marketing names some fastboot builds report as "product" (#129).
var boardAliases = map[string]string{"prime4plus": "jc11s", "prime4+": "jc11s"}

func boardCode(code string) string {
	code = strings.ToLower(strings.TrimSpace(code))
	code = strings.TrimPrefix(code, "inmusic,")
	if a, ok := boardAliases[strings.ReplaceAll(code, " ", "")]; ok {
		return a
	}
	if _, ok := gen2Boards[code]; ok {
		return code
	}
	if _, ok := gen1Boards[code]; ok {
		return code
	}
	// Fastboot product strings often include SoC/platform prefixes, e.g. "rk3288-az05-jp11s".
	// Match on the last token after '-' or '_'.
	if idx := strings.LastIndexAny(code, "-_/"); idx >= 0 {
		sub := strings.TrimPrefix(code[idx+1:], "inmusic,")
		if _, ok := gen2Boards[sub]; ok {
			return sub
		}
		if _, ok := gen1Boards[sub]; ok {
			return sub
		}
	}
	return code
}

func lookupBoard(code string) (m model, gen2, ok bool) {
	c := boardCode(code)
	if m, ok = gen2Boards[c]; ok {
		return m, true, true
	}
	m, ok = gen1Boards[c]
	return m, false, ok
}

// ---- discovery --------------------------------------------------------------

type announce struct {
	Source, Action, Software, Version string
	Port                              uint16
}

// parseAirD reads a StageLinQ discovery packet: "airD", a 16-byte token, four
// length-prefixed UTF-16BE strings (device, action, software, version), a port.
func parseAirD(b []byte) (a announce, err error) {
	if len(b) < 20 || string(b[:4]) != "airD" {
		return a, errors.New("not a StageLinQ discovery packet")
	}
	b = b[20:]
	var s [4]string
	for i := range s {
		if len(b) < 4 {
			return a, errors.New("truncated airD packet")
		}
		n := binary.BigEndian.Uint32(b)
		b = b[4:]
		if n%2 != 0 || uint64(n) > uint64(len(b)) {
			return a, errors.New("bad airD string length")
		}
		u := make([]uint16, n/2)
		for j := range u {
			u[j] = binary.BigEndian.Uint16(b[2*j:])
		}
		s[i], b = string(utf16.Decode(u)), b[n:]
	}
	if len(b) < 2 {
		return a, errors.New("truncated airD packet")
	}
	return announce{s[0], s[1], s[2], s[3], binary.BigEndian.Uint16(b)}, nil
}

type deck struct {
	IP, Name, Board  string
	Announced, Known bool // Known: a deck we can act on (its board code is ours, or it answers like EngineX)
	FromEAAS         bool // unauthenticated discovery hint; never a board identity
}

func isLocalIP(ip string) bool {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			if n.IP.Equal(parsed) {
				return true
			}
		}
	}
	return false
}

// listenAirD collects the decks that announce themselves for o.AirDWait.
func listenAirD(o *options) map[string]announce {
	found := map[string]announce{}
	lc := net.ListenConfig{Control: setReuseControl}
	pc, err := lc.ListenPacket(context.Background(), "udp4", o.AirDAddr)
	if err != nil {
		o.say("无法监听 StageLinQ 端口（"+err.Error()+"），只用局域网扫描。", "Cannot listen for StageLinQ ("+err.Error()+"); scanning only.")
		return found
	}
	defer pc.Close()
	pc.SetReadDeadline(time.Now().Add(o.AirDWait))
	buf := make([]byte, 2048)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			return found
		}
		a, err := parseAirD(buf[:n])
		if err != nil || strings.Contains(a.Action, "EXIT") {
			continue
		}
		found[from.(*net.UDPAddr).IP.String()] = a
	}
}

// isEAAS accepts only the six-byte beacon captured on the Mixstream Pro+ (#164).
// It carries no model or authentication information.
func isEAAS(b []byte) bool {
	return len(b) == 6 && string(b) == "EAAS\x01\x00"
}

// listenEAAS collects candidate IPs, not verified decks or model identities.
func listenEAAS(o *options) map[string]bool {
	found := map[string]bool{}
	lc := net.ListenConfig{Control: setReuseControl}
	pc, err := lc.ListenPacket(context.Background(), "udp4", o.EAASAddr)
	if err != nil {
		o.say("无法监听 EAAS 端口（"+err.Error()+"），只用 StageLinQ 与扫描。", "Cannot listen for EAAS ("+err.Error()+"); scanning only.")
		return found
	}
	defer pc.Close()
	pc.SetReadDeadline(time.Now().Add(o.AirDWait))
	// Hold any UDP datagram: Windows reports WSAEMSGSIZE on truncation, which
	// would otherwise stop discovery when unrelated larger broadcasts arrive.
	buf := make([]byte, 65535)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			return found
		}
		if isEAAS(buf[:n]) {
			udpAddr, ok := from.(*net.UDPAddr)
			if !ok {
				continue
			}
			ip := udpAddr.IP.String()
			// Bound memory even if many spoofed source addresses send valid beacons.
			if len(found) < 256 && !udpAddr.IP.IsUnspecified() && !udpAddr.IP.IsMulticast() && !isLocalIP(ip) {
				found[ip] = true
			}
		}
	}
}

func localSubnets() []string {
	var out []string
	seen := map[string]bool{}
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		n, ok := a.(*net.IPNet)
		if !ok || n.IP.IsLoopback() || n.IP.To4() == nil {
			continue
		}
		p := strings.Join(strings.Split(n.IP.String(), ".")[:3], ".")
		if !seen[p] {
			seen[p], out = true, append(out, p)
		}
	}
	return out
}

// isDeckUI is true when host is EngineX's web UI: /v1/sc5000/ui answers with a JSON object, or
// (403/401 when the console asks for its access code) the login page on / is the Stream Sync one (#164).
func isDeckUI(c *http.Client, host string, port int) bool {
	resp, err := c.Get(fmt.Sprintf("http://%s:%d/v1/sc5000/ui", host, port))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode == http.StatusOK {
		return bytes.HasPrefix(bytes.TrimSpace(body), []byte("{"))
	}
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusUnauthorized {
		return false
	}
	page, err := c.Get(fmt.Sprintf("http://%s:%d/", host, port))
	if err != nil {
		return false
	}
	defer page.Body.Close()
	html, _ := io.ReadAll(io.LimitReader(page.Body, 2048))
	return page.StatusCode == http.StatusOK && bytes.Contains(html, []byte("<title>Stream Sync</title>"))
}

// scanLAN probes every host of the /24s with 64 workers.
// shortcut: only EngineX's web UI is probed, so a deck without it is found by StageLinQ
// alone; SSH banners are not scanned, as a bare port 22 says nothing about a deck.
func scanLAN(o *options) []string {
	prefixes := o.Subnets
	if prefixes == nil {
		prefixes = localSubnets()
	}
	c := &http.Client{Timeout: 400 * time.Millisecond, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	jobs := make(chan string)
	var mu sync.Mutex
	var hits []string
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for h := range jobs {
				if isDeckUI(c, h, o.UIPort) {
					mu.Lock()
					hits = append(hits, h)
					mu.Unlock()
				}
			}
		}()
	}
	for _, p := range prefixes {
		for i := 1; i < 255; i++ {
			jobs <- fmt.Sprintf("%s.%d", p, i)
		}
	}
	close(jobs)
	wg.Wait()
	return hits
}

func discover(ctx context.Context, o *options) []deck {
	var mu sync.Mutex
	byIP := map[string]*deck{}
	get := func(ip string) *deck {
		if byIP[ip] == nil {
			byIP[ip] = &deck{IP: ip}
		}
		return byIP[ip]
	}
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for ip, a := range listenAirD(o) {
			mu.Lock()
			d := get(ip)
			d.Announced, d.Name = true, a.Source
			if _, _, ok := lookupBoard(a.Software); ok { // decks announce their board code as the software name
				d.Board, d.Known = boardCode(a.Software), true
			}
			mu.Unlock()
		}
	}()
	go func() {
		defer wg.Done()
		for ip := range listenEAAS(o) {
			mu.Lock()
			d := get(ip)
			d.Announced = true
			d.FromEAAS = true
			if d.Name == "" {
				d.Name = "Unverified EAAS device"
			}
			mu.Unlock()
		}
	}()
	go func() {
		defer wg.Done()
		for _, ip := range scanLAN(o) {
			mu.Lock()
			get(ip).Known = true
			mu.Unlock()
		}
	}()
	wg.Wait()
	var out []deck
	for _, d := range byIP {
		out = append(out, *d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].IP < out[j].IP })
	return out
}

// ---- bundle -----------------------------------------------------------------

type bundleInfo struct {
	Path, Top, Version string
	HeaderPrefix       string // model prefix in the 128-byte V1 header when Path is *-Update.img
	IsImage            bool
	TarGz              []byte // verified inner Data-Bundle.tar.gz payload
	HostScript         []byte // host-install-userdata.py, shipped inside the bundle
	USBScript          []byte // host-install-usbupdate.py (mode C), in bundles since #116
}

var (
	topRE   = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	hex64RE = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)
)

func isGen2UpdateImgName(name, prefix string) bool {
	return strings.HasPrefix(name, prefix+"-") && strings.Contains(name, "-EngineX-OS-") && strings.HasSuffix(name, "-Update.img")
}

func isGen2BundleName(name, prefix string) bool {
	return strings.HasPrefix(name, prefix+"-") && strings.HasSuffix(name, bundleSuffix)
}

// unwrapGen2ImageOrBundle verifies and strips the 128-byte ENGINEX_DATA_BUNDLE_IMG_V1 header
// from a 2nd-generation *-Update.img, or returns a raw .tar.gz bundle unchanged (#215).
func unwrapGen2ImageOrBundle(data []byte) ([]byte, error) {
	payload, _, _, err := parseGen2ImageOrBundle(data)
	return payload, err
}

func parseGen2ImageOrBundle(data []byte) (tarGz []byte, headerPrefix string, isImage bool, err error) {
	if bytes.HasPrefix(data, []byte(gen2ImgPrefix)) {
		if len(data) <= gen2ImgHeader || string(data[:len(gen2ImgMagic)]) != gen2ImgMagic || data[len(gen2ImgMagic)+64] != '\n' {
			return nil, "", true, errors.New("damaged Gen-2 Update.img: truncated or invalid ENGINEX_DATA_BUNDLE_IMG_V1 header")
		}
		hdr := string(bytes.TrimRight(data[:gen2ImgHeader], "\x00"))
		if !strings.HasSuffix(hdr, "\n") {
			return nil, "", true, errors.New("damaged Gen-2 Update.img: invalid ENGINEX_DATA_BUNDLE_IMG_V1 header")
		}
		lines := strings.Split(strings.TrimSuffix(hdr, "\n"), "\n")
		if len(lines) != 4 || lines[0] != "ENGINEX_DATA_BUNDLE_IMG_V1" || !hex64RE.MatchString(lines[1]) || lines[2] == "" || lines[3] == "" {
			return nil, "", true, errors.New("damaged Gen-2 Update.img: invalid ENGINEX_DATA_BUNDLE_IMG_V1 header fields")
		}
		sum := sha256.Sum256(data[gen2ImgHeader:])
		got := hex.EncodeToString(sum[:])
		want := strings.ToLower(lines[1])
		if got != want {
			return nil, "", true, fmt.Errorf("damaged Gen-2 Update.img: payload SHA-256 mismatch (got %s, header says %s)", got, want)
		}
		return data[gen2ImgHeader:], lines[2], true, nil
	}
	if len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b {
		return data, "", false, nil
	}
	return nil, "", false, errors.New("unrecognized Gen-2 image or bundle format (expected ENGINEX_DATA_BUNDLE_IMG_V1 or .tar.gz)")
}

// readBundle walks the Gen-2 *-Update.img or .tar.gz once: header SHA-256 (if .img), one top directory,
// and every file inside matches SHA256SUMS.
func readBundle(path string) (*bundleInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	const maxRead = bundleMaxDown + gen2ImgHeader
	raw, err := io.ReadAll(io.LimitReader(f, maxRead+1))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	if int64(len(raw)) > maxRead {
		return nil, fmt.Errorf("%s: exceeds size limit", filepath.Base(path))
	}
	tarGz, hdrPrefix, isImg, err := parseGen2ImageOrBundle(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(tarGz))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	defer gz.Close()
	b := &bundleInfo{Path: path, HeaderPrefix: hdrPrefix, IsImage: isImg, TarGz: tarGz}
	sums := map[string]string{}
	var listed, version []byte
	var total int64
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
		}
		name := strings.TrimPrefix(h.Name, "./")
		if b.Top == "" {
			b.Top = strings.SplitN(name, "/", 2)[0]
			if !topRE.MatchString(b.Top) {
				return nil, fmt.Errorf("%s: unsafe top directory %q", filepath.Base(path), b.Top)
			}
		}
		rel := strings.TrimPrefix(name, b.Top)
		if rel != "" && !strings.HasPrefix(rel, "/") || strings.Contains(name, "..") {
			return nil, fmt.Errorf("%s: %q is outside %s/", filepath.Base(path), h.Name, b.Top)
		}
		rel = strings.TrimPrefix(rel, "/")
		if h.Typeflag != '0' && h.Typeflag != 0 { // directories, pax headers
			continue
		}
		hash := sha256.New()
		var keep bytes.Buffer
		w := io.Writer(hash)
		if rel == "SHA256SUMS" || rel == "version.txt" || rel == "host-install-userdata.py" || rel == "host-install-usbupdate.py" {
			w = io.MultiWriter(hash, &keep)
		}
		n, err := io.Copy(w, tr)
		if total += n; err != nil || total > bundleMaxUnpack {
			return nil, fmt.Errorf("%s: unpacks too large or damaged", filepath.Base(path))
		}
		sums[rel] = hex.EncodeToString(hash.Sum(nil))
		switch rel {
		case "SHA256SUMS":
			listed = keep.Bytes()
		case "version.txt":
			version = keep.Bytes()
		case "host-install-userdata.py":
			b.HostScript = keep.Bytes()
		case "host-install-usbupdate.py":
			b.USBScript = keep.Bytes()
		}
	}
	if len(listed) == 0 {
		return nil, fmt.Errorf("%s: SHA256SUMS missing in the bundle", filepath.Base(path))
	}
	for _, line := range strings.Split(string(listed), "\n") {
		if f := strings.Fields(line); len(f) == 2 && sums[f[1]] != f[0] {
			return nil, fmt.Errorf("%s: SHA256SUMS mismatch for %s, the bundle is damaged", filepath.Base(path), f[1])
		}
	}
	if b.Version = strings.TrimSpace(string(version)); b.Version == "" {
		return nil, fmt.Errorf("%s: no version.txt", filepath.Base(path))
	}
	return b, nil
}

type release struct {
	Tag    string `json:"tag_name"`
	Assets []struct {
		Name   string `json:"name"`
		URL    string `json:"browser_download_url"`
		Digest string `json:"digest"`
	} `json:"assets"`
}

// fetchBundle downloads the latest release's Gen-2 *-Update.img (or legacy Data-Bundle.tar.gz fallback)
// for prefix into dir and checks GitHub's SHA-256 (#215).
func fetchBundle(ctx context.Context, o *options, prefix, dir string) (string, error) {
	var rel release
	if err := getJSON(ctx, o, o.API+"/repos/"+o.Repo+"/releases/latest", &rel); err != nil {
		return "", fmt.Errorf("GitHub release: %w", err)
	}
	pick := func(match func(string) bool) (name, url, digest string, ok bool) {
		for _, a := range rel.Assets {
			if match(a.Name) && strings.HasPrefix(a.URL, "https://") {
				return a.Name, a.URL, a.Digest, true
			}
		}
		return "", "", "", false
	}
	name, url, digest, ok := pick(func(n string) bool { return isGen2UpdateImgName(n, prefix) })
	if !ok {
		name, url, digest, ok = pick(func(n string) bool { return isGen2BundleName(n, prefix) })
	}
	if !ok {
		return "", fmt.Errorf("release %s has no %s-*-EngineX-OS-*-Update.img or %s-*%s", rel.Tag, prefix, prefix, bundleSuffix)
	}
	o.say("下载 "+name+" ("+rel.Tag+")", "Downloading "+name)
	dest := filepath.Join(dir, filepath.Base(name))
	if err := download(ctx, o, url, strings.TrimPrefix(digest, "sha256:"), dest); err != nil {
		return "", err
	}
	return dest, nil
}

// fetchImage requires a release-provided digest before handling firmware.
func fetchImage(ctx context.Context, o *options, prefix string) (string, error) {
	var rel release
	if err := getJSON(ctx, o, o.API+"/repos/"+o.Repo+"/releases/latest", &rel); err != nil {
		return "", err
	}
	for _, a := range rel.Assets {
		if !strings.HasPrefix(a.Name, prefix+"-") || !strings.Contains(a.Name, "-EngineX-OS-") || !strings.HasSuffix(a.Name, "-Update.img") || !strings.HasPrefix(a.URL, "https://") {
			continue
		}
		digest := strings.TrimPrefix(a.Digest, "sha256:")
		if !strings.HasPrefix(a.Digest, "sha256:") || len(digest) != 64 {
			return "", errors.New("release image has no SHA-256 digest")
		}
		path := filepath.Join(o.WorkDir, filepath.Base(a.Name))
		if err := downloadLimit(ctx, o, a.URL, digest, path, 2<<30); err != nil {
			return "", err
		}
		return path, nil
	}
	return "", fmt.Errorf("release %s has no %s update image", rel.Tag, prefix)
}

func getJSON(ctx context.Context, o *options, url string, v any) error {
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "enginex-installer")
	resp, err := o.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(v)
}

func download(ctx context.Context, o *options, url, wantSHA, dest string) error {
	return downloadLimit(ctx, o, url, wantSHA, dest, bundleMaxDown+gen2ImgHeader)
}

func downloadLimit(ctx context.Context, o *options, url, wantSHA, dest string, limit int64) error {
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	req.Header.Set("User-Agent", "enginex-installer")
	resp, err := o.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("download: HTTP %d", resp.StatusCode)
	}
	part := dest + ".part"
	f, err := os.Create(part)
	if err != nil {
		return err
	}
	defer os.Remove(part)
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, limit+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if n > limit {
		return errors.New("download exceeds size limit")
	}
	if got := hex.EncodeToString(h.Sum(nil)); wantSHA == "" {
		o.say("Release 没有提供 SHA-256，只校验包内 SHA256SUMS。", "The release gives no SHA-256; only the bundle's own SHA256SUMS is checked.")
	} else if !strings.EqualFold(got, wantSHA) {
		return fmt.Errorf("download SHA-256 mismatch: got %s, release says %s", got, wantSHA)
	}
	return os.Rename(part, dest)
}

// obtainBundle uses --bundle/--image, else the newest matching *-Update.img or Data-Bundle.tar.gz
// in the work dir, else the latest release (#215).
func obtainBundle(ctx context.Context, o *options, prefix string) (*bundleInfo, error) {
	path := o.Bundle
	if path != "" {
		if !strings.HasPrefix(filepath.Base(path), prefix+"-") {
			return nil, fmt.Errorf("%s is not a %s image/bundle: wrong model refused", filepath.Base(path), prefix)
		}
	} else {
		local, _ := filepath.Glob(filepath.Join(o.WorkDir, prefix+"-*-EngineX-OS-*-Update.img"))
		if len(local) == 0 {
			local, _ = filepath.Glob(filepath.Join(o.WorkDir, prefix+"-*"+bundleSuffix))
		}
		sort.Slice(local, func(i, j int) bool { return mtime(local[i]).After(mtime(local[j])) })
		if len(local) > 0 {
			path = local[0]
			o.say("使用已有的 "+filepath.Base(path), "Using the package already here: "+filepath.Base(path))
		} else {
			var err error
			if path, err = fetchBundle(ctx, o, prefix, o.WorkDir); err != nil {
				return nil, err
			}
		}
	}
	b, err := readBundle(path)
	if err != nil {
		return nil, err
	}
	if b.HeaderPrefix != "" && b.HeaderPrefix != prefix {
		return nil, fmt.Errorf("%s header is for %s, not %s: wrong model refused", filepath.Base(path), b.HeaderPrefix, prefix)
	}
	return b, nil
}

func mtime(p string) time.Time {
	st, err := os.Stat(p)
	if err != nil {
		return time.Time{}
	}
	return st.ModTime()
}

// ---- mode A: SSH ------------------------------------------------------------

func dialSSH(o *options, ip string) (*ssh.Client, error) {
	var auth []ssh.AuthMethod
	var signers []ssh.Signer
	keys := []string{o.Key}
	if o.Key == "" {
		home, _ := os.UserHomeDir()
		keys = []string{filepath.Join(home, ".ssh/id_ed25519"), filepath.Join(home, ".ssh/id_rsa"), filepath.Join(home, ".ssh/id_ecdsa")}
	}
	for _, k := range keys {
		pem, err := os.ReadFile(k)
		if err != nil {
			if o.Key != "" {
				return nil, err
			}
			continue
		}
		if s, err := ssh.ParsePrivateKey(pem); err == nil {
			signers = append(signers, s)
		} else if o.Key != "" {
			return nil, fmt.Errorf("%s: %w", k, err)
		}
	}
	if len(signers) > 0 {
		auth = append(auth, ssh.PublicKeys(signers...))
	}
	passwords := []string{"denon"} // FreeMyPrime-unlocked decks
	if o.Password != "" {
		passwords = []string{o.Password, "denon"}
	}
	for _, p := range passwords {
		auth = append(auth, ssh.Password(p))
	}
	cfg := &ssh.ClientConfig{User: "root", Auth: auth, Timeout: 5 * time.Second,
		// shortcut: decks are reflashed and get new host keys, and this is a LAN install; pin keys if this ever runs over a WAN.
		HostKeyCallback: ssh.InsecureIgnoreHostKey()}
	return ssh.Dial("tcp", net.JoinHostPort(ip, strconv.Itoa(o.SSHPort)), cfg)
}

func isAuthErr(err error) bool { return strings.Contains(err.Error(), "unable to authenticate") }

func sshRun(c *ssh.Client, cmd string, stdin io.Reader) (string, error) {
	s, err := c.NewSession()
	if err != nil {
		return "", err
	}
	defer s.Close()
	s.Stdin = stdin
	out, err := s.CombinedOutput(cmd) // not one Buffer for both pipes: their copiers race on it
	return string(out), err
}

// installCmd unpacks the streamed tarball in the deck's temp dir, runs install.sh, and marks gen2-unlocked (#215).
func installCmd(top string) string {
	return fmt.Sprintf(`set -e; cd "${TMPDIR:-/tmp}"; trap 'rm -rf %[1]s' EXIT; rm -rf %[1]s; tar -xzf -; sh %[1]s/install.sh; printf '1\n' > %[2]s/gen2-unlocked; rm -f %[2]s/gen2-locked`, top, customDirSh)
}

func parseCompatible(out string) (code string, m model, gen2, ok bool) {
	for _, c := range strings.Split(out, "\x00") {
		if strings.HasPrefix(c, "inmusic,") {
			if m, gen2, ok = lookupBoard(c); ok {
				return boardCode(c), m, gen2, true
			}
		}
	}
	return "", model{}, false, false
}

func modeA(ctx context.Context, o *options, d deck) error {
	label := fmt.Sprintf("%s (%s)", d.Name, d.IP)
	// An EAAS beacon does not prove this is a deck. Even a matching HTTP response
	// must not upgrade this hint into permission to send SSH credentials (#164).
	if d.FromEAAS && d.Board == "" {
		if o.Uninstall {
			return fmt.Errorf("%s: 收到未验证的 EAAS 广播，无法执行网络卸载 / received unverified EAAS beacon, cannot uninstall over SSH", label)
		}
		return modeAB(ctx, o, d)
	}
	cl, err := dialSSH(o, d.IP)
	if err != nil {
		if o.Uninstall {
			return fmt.Errorf("%s: 卸载需通过 SSH 连接碟机，无法连接 SSH / --uninstall requires SSH access to the deck, cannot connect: %w", label, err)
		}
		if isAuthErr(err) {
			return fmt.Errorf("%s: SSH 登录失败 / SSH login failed; use --key or --password", label)
		}
		return modeAB(ctx, o, d)
	}
	defer cl.Close()
	out, err := sshRun(cl, compatCmd, nil)
	if err != nil {
		return fmt.Errorf("%s: cannot read the board: %w", label, err)
	}
	code, m, gen2, ok := parseCompatible(out)
	if !ok {
		return fmt.Errorf("%s: 未知型号 / unknown board %q", label, strings.TrimSpace(out))
	}
	if o.Uninstall {
		if !gen2 {
			return fmt.Errorf("%s (%s): 一代机请使用 U 盘刷回官方固件 / 1st-generation deck; copy the stock firmware *.img to a USB stick to revert to stock", label, m.Name)
		}
		if err := confirm(o, fmt.Sprintf("确认从 %s 卸载 EngineX OS 并恢复原厂系统？", label), fmt.Sprintf("Uninstall EngineX OS from %s and revert to stock firmware?", label)); err != nil {
			return err
		}
		// A deck whose bundle predates 5.3.0.3 never ran the #103 migration: its install is still
		// the real /data/sc5000-custom, and there is no /data/enginex to run uninstall.sh from.
		uninstallCmd := `d=` + customDirSh + `; [ -f "$d/bundle/uninstall.sh" ] || d=$(dirname "$d")/sc5000-custom; ` +
			`sh "$d/bundle/uninstall.sh" && (systemctl reboot || reboot || :)`
		if out, err := sshRun(cl, uninstallCmd, nil); err != nil {
			return fmt.Errorf("%s: uninstall failed: %w\n%s", label, err, out)
		}
		o.say("已从 /data 卸载 EngineX OS，碟机正在重启回到原厂系统",
			"EngineX OS removed from /data; the deck is rebooting to stock firmware")
		return nil
	}
	if !gen2 {
		path, err := fetchImage(ctx, o, m.Prefix)
		if err != nil {
			return err
		}
		if err := confirm(o, "将更新一代机 "+m.Name+"，请确认已备份数据。", "Update "+m.Name+"; confirm you have backed up your data."); err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			return err
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return err
		}
		cmd := fmt.Sprintf("set -e; cat > /data/az01-update.img; echo '%x  /data/az01-update.img' | sha256sum -c -; /usr/sbin/az01-update --file /data/az01-update.img", h.Sum(nil))
		out, err := sshRun(cl, cmd, f)
		if err != nil {
			return fmt.Errorf("update failed: %w\n%s", err, out)
		}
		return nil
	}
	o.say(fmt.Sprintf("发现 %s，型号 %s (%s)，在线安装，保留所有音乐和曲库。", label, m.Name, code),
		"Found "+label+", "+m.Name+"; installing online, your music and library stay.")
	b, err := obtainBundle(ctx, o, m.Prefix)
	if err != nil {
		return err
	}
	sayGen2Steps(o, filepath.Base(b.Path))
	if out, err := sshRun(cl, installCmd(b.Top), bytes.NewReader(b.TarGz)); err != nil {
		return fmt.Errorf("%s: install failed: %w\n%s", label, err, out)
	}
	got, err := sshRun(cl, "cat "+customDirSh+"/version.txt", nil)
	if err != nil || strings.TrimSpace(got) != b.Version {
		return fmt.Errorf("%s: installed version reads back as %q, expected %s", label, strings.TrimSpace(got), b.Version)
	}
	if out, err := sshRun(cl, `sh `+customDirSh+`/bundle/boot-activate.sh`, nil); err != nil {
		return fmt.Errorf("%s: activation failed: %w\n%s", label, err, out)
	}
	o.say(fmt.Sprintf("完成：%s 已安装 EngineX OS %s，Engine 正在重启。", label, b.Version),
		fmt.Sprintf("Done: EngineX OS %s installed on %s; Engine is restarting.", b.Version, label))
	sayGen2Unlocked(o, b, m.Prefix)
	return nil
}

// ---- mode B: fastboot -------------------------------------------------------

// fbVIDs are tried with -i when plain "fastboot devices" sees nothing: old fastboot builds only
// list whitelisted vendors. 0x15e4 is what every 2nd-gen U-Boot DTB sets (inmusic,usb-vendor), 0x2207 is Rockchip (#129).
var fbVIDs = []string{"0x15e4", "0x2207"}

// geteuid and goos are seams for tests (they may run as root on the build host).
var (
	geteuid = os.Geteuid
	goos    = runtime.GOOS
)

// findFastboot prefers a fastboot binary shipped beside the installer, else the one in PATH.
func findFastboot() string {
	name := "fastboot"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if exe, err := os.Executable(); err == nil {
		if p := filepath.Join(filepath.Dir(exe), name); fileExists(p) {
			return p
		}
	}
	return "fastboot"
}

func haveFastboot(o *options) bool { _, err := exec.LookPath(o.Fastboot); return err == nil }

// fastbootMissing tells the user how to get fastboot on this OS (#129: "bash: fastboot: command not found").
func fastbootMissing() error {
	var how string
	switch runtime.GOOS {
	case "windows":
		how = "https://dl.google.com/android/repository/platform-tools-latest-windows.zip -> fastboot.exe, AdbWinApi.dll, AdbWinUsbApi.dll"
	case "darwin":
		how = "brew install --cask android-platform-tools"
	default:
		how = "sudo apt install fastboot  |  sudo dnf install android-tools  |  sudo pacman -S android-tools"
	}
	return errors.New("没有找到 fastboot 程序：请先安装，或把 fastboot 放在本工具同一目录后重新运行 / fastboot is not installed: install it, or put the fastboot binary next to this tool, then run again:\n  " + how)
}

func formatBytes(b int64) string {
	switch {
	case b >= 1024*1024*1024:
		return fmt.Sprintf("%.1f GB", float64(b)/(1024*1024*1024))
	case b >= 1024*1024:
		return fmt.Sprintf("%.1f MB", float64(b)/(1024*1024))
	case b >= 1024:
		return fmt.Sprintf("%.1f KB", float64(b)/1024)
	default:
		return fmt.Sprintf("%d B", b)
	}
}

func fb(ctx context.Context, o *options, args ...string) (string, error) {
	if o.fbVID != "" {
		args = append([]string{"-i", o.fbVID}, args...)
	}
	out, err := exec.CommandContext(ctx, o.Fastboot, args...).CombinedOutput()
	return string(out), err
}

type captureWriter struct {
	buf *bytes.Buffer
	out io.Writer
}

func (w *captureWriter) Write(p []byte) (int, error) {
	n, _ := w.buf.Write(p)
	if w.out != nil {
		_, _ = w.out.Write(p) // display error must never abort fastboot or drop buffer data (#129)
	}
	return n, nil
}

// fbLive runs fastboot and streams stdout and stderr directly to o.Out in real time while also
// capturing the full output, so the user sees live progress during long flash operations (#129).
func fbLive(ctx context.Context, o *options, args ...string) (string, error) {
	if o.fbVID != "" {
		args = append([]string{"-i", o.fbVID}, args...)
	}
	cmd := exec.CommandContext(ctx, o.Fastboot, args...)
	cw := &captureWriter{buf: &bytes.Buffer{}, out: o.Out}
	cmd.Stdout = cw
	cmd.Stderr = cw
	err := cmd.Run()
	return cw.buf.String(), err
}

const udevHint = `printf 'SUBSYSTEM=="usb", ATTR{idVendor}=="%s", MODE="0666"\n' 15e4 1cc5 2207 18d1 | sudo tee /etc/udev/rules.d/99-enginex.rules && sudo udevadm control --reload-rules && sudo udevadm trigger`

// fastbootDevices lists fastboot serials; if none show up it retries with each of fbVIDs and,
// when one finds the deck, keeps that -i for every later fastboot call.
func fastbootDevices(ctx context.Context, o *options) []string {
	serials, _ := fastbootList(ctx, o)
	if len(serials) > 0 || o.fbVID != "" {
		return serials
	}
	for _, v := range fbVIDs {
		o.fbVID = v
		var out string
		if serials, out = fastbootList(ctx, o); len(serials) > 0 {
			return serials
		}
		if strings.Contains(out, "invalid option") || strings.Contains(out, "unknown option") { // new platform-tools dropped -i (#129, #164)
			break
		}
	}
	o.fbVID = ""
	return nil
}

// fastbootList parses "fastboot devices": "<serial>\tfastboot", or "<serial>\tAndroid Fastboot [usb:3-3.1]"
// from newer android-tools (#129). It also returns the raw output.
func fastbootList(ctx context.Context, o *options) ([]string, string) {
	out, err := fb(ctx, o, "devices")
	if l := strings.ToLower(out); geteuid() != 0 && (strings.Contains(l, "no permissions") || strings.Contains(l, "permission denied")) {
		o.hint("perm", "fastboot 看到了碟机，但没有 USB 权限。请用 sudo 运行本工具，或执行下面的命令添加 udev 规则后重新插拔 USB：\n  "+udevHint,
			"fastboot sees the deck but has no USB permission. Run this tool with sudo, or add a udev rule and replug USB:\n  "+udevHint)
	}
	if err != nil {
		return nil, out
	}
	var serials []string
	for _, l := range strings.Split(out, "\n") {
		if ll := strings.ToLower(l); strings.Contains(ll, "no permissions") || strings.Contains(ll, "permission denied") {
			continue
		}
		f := strings.Fields(l)
		for i := 1; i < len(f) && i <= 2; i++ {
			if strings.HasPrefix(strings.ToLower(f[i]), "fastboot") {
				serials = append(serials, f[0])
				break
			}
		}
	}
	return serials, out
}

// IsRockchipUSBLoader reports whether line describes a Rockchip USB loader device:
// 2207:350b (RK3588 RMZ2/JP24) or 2207:320a (RK3288).
func IsRockchipUSBLoader(line string) bool {
	lower := strings.ToLower(line)
	return strings.Contains(lower, "2207:350b") || strings.Contains(lower, "2207:320a")
}

// gen2USB is the USB id each 2nd-gen U-Boot DTB sets (inmusic,usb-vendor/-product); Engine keeps the
// same id in normal operation, where RattyDAVE's PRIME 4+ showed 15e4:d007 (#129).
// Mixstream Pro+ uses 15e4:103f (computer mode) and 15e4:303f (firmware update mode),
// both captured on NH08S in #164. The official 5.3.0 Mixstream image that both NH08S and NH10
// accept lists usb_devices 15e4:303f and 15e4:2059, so 2059 is taken as NH10's update-mode id
// (not yet seen on a real NH10; modeB still reads the model from fastboot before flashing).
var gen2USB = map[string]string{
	"15e4:d007": "jc11s", "15e4:c00c": "jp11s", "15e4:d011": "jp20", "15e4:d012": "jp21",
	"15e4:103f": "nh08s", "15e4:303f": "nh08s", "15e4:2059": "nh10",
}

// gen1USB is the usb_devices table of the official 5.3.0 1st-gen images, in their compatible order.
// Their fastboot has no "getvar product" ("Variable not implemented" on an SC5000), so modeB
// names the deck by this id; all boards of one image group get the same image anyway.
var gen1USB = map[string]string{
	"15e4:d006": "jp07", "15e4:d00a": "jp08", "15e4:d00c": "jp11", "15e4:d00d": "jp13", "15e4:d00e": "jp14",
	"15e4:d008": "jc11", "15e4:d00b": "jc16", "15e4:203f": "nh08",
}

var vidPID = regexp.MustCompile(`(?i)\b[0-9a-f]{4}:[0-9a-f]{4}\b`)

// usbDeck returns the board and host USB line of the first deck from ids on the bus.
// The first VID:PID on the line identifies the device, so product names cannot impersonate a deck.
func usbDeck(ctx context.Context, o *options, ids map[string]string) (board, line string) {
	for _, l := range strings.Split(o.ListUSB(ctx), "\n") {
		// The first VID:PID identifies the device; later IDs may be part of its name.
		if id := vidPID.FindString(l); id != "" {
			if b := ids[strings.ToLower(id)]; b != "" {
				return b, strings.TrimSpace(l)
			}
		}
	}
	return "", ""
}

// isDeckUSB reports whether a host USB line looks like a 2nd-gen deck in an update mode (#129):
// a Rockchip loader, Rockchip / Google fastboot ids, or inMusic / PRIME 4 / SC LIVE / Mixstream product names.
func isDeckUSB(line string) bool {
	lower := strings.ToLower(line)
	id := vidPID.FindString(lower)
	return IsRockchipUSBLoader(lower) || gen2USB[id] != "" || id == "2207:0006" || id == "18d1:4ee0" ||
		strings.Contains(lower, "inmusic") || strings.Contains(lower, "prime 4") || strings.Contains(lower, "sc live") || strings.Contains(lower, "mixstream")
}

var (
	pnpBlockRE = regexp.MustCompile(`\n[ \t]*\n`)
	winVIDPID  = regexp.MustCompile(`(?i)VID_([0-9a-f]{4})&PID_([0-9a-f]{4})`)
	descKeyRE  = regexp.MustCompile(`(?i)(description|opis|beschreibung|descrip|descriz|описание|描述|説明|설명)`)
	macVID     = regexp.MustCompile(`"idVendor" = (\d+)`)
	macPID     = regexp.MustCompile(`"idProduct" = (\d+)`)
)

// hostUSB lists the host's USB devices as "vid:pid name" lines, best effort.
func hostUSB(ctx context.Context) string {
	switch runtime.GOOS {
	case "windows":
		out, _ := exec.CommandContext(ctx, "pnputil", "/enum-devices", "/connected").Output()
		return parsePnPUtil(string(out))
	case "darwin":
		out, _ := exec.CommandContext(ctx, "ioreg", "-p", "IOUSB", "-l", "-w0").Output()
		return parseIOReg(string(out))
	}
	dirs, _ := filepath.Glob("/sys/bus/usb/devices/*")
	var b strings.Builder
	for _, d := range dirs {
		rd := func(n string) string { s, _ := os.ReadFile(filepath.Join(d, n)); return strings.TrimSpace(string(s)) }
		if v := rd("idVendor"); v != "" {
			fmt.Fprintf(&b, "%s:%s %s %s\n", v, rd("idProduct"), rd("manufacturer"), rd("product"))
		}
	}
	return b.String()
}

// parsePnPUtil turns `pnputil /enum-devices /connected` output into "vid:pid name" lines.
// It works across all Windows languages (English, Polish, German, French, Chinese, etc.) by extracting
// VID_xxxx&PID_yyyy and the device description from following lines in the device block.
func parsePnPUtil(out string) string {
	var b strings.Builder
	// pnputil emits one blank-line-separated block per device, with its instance
	// path first. Only that path can identify the device; descriptions and other
	// fields may themselves contain VID/PID text supplied by an unrelated device.
	for _, block := range pnpBlockRE.Split(strings.TrimSpace(strings.ReplaceAll(out, "\r\n", "\n")), -1) {
		lines := strings.Split(strings.TrimSpace(block), "\n")
		_, instance, ok := strings.Cut(strings.TrimSpace(lines[0]), ":")
		instance = strings.TrimSpace(instance)
		if !ok || !strings.HasPrefix(strings.ToUpper(instance), `USB\VID_`) {
			continue
		}
		m := winVIDPID.FindStringSubmatch(instance)
		if m == nil {
			continue
		}
		desc, fallback := "", ""
		for _, line := range lines[1:] {
			key, val, ok := strings.Cut(strings.TrimSpace(line), ":")
			if !ok {
				continue
			}
			val = strings.TrimSpace(val)
			if val == "" || strings.HasPrefix(val, "{") || strings.HasSuffix(strings.ToLower(val), ".inf") {
				continue
			}
			if strings.EqualFold(val, "started") || strings.EqualFold(val, "stopped") || strings.EqualFold(val, "disconnected") {
				continue
			}
			if descKeyRE.MatchString(key) {
				desc = val
				break
			}
			if fallback == "" {
				fallback = val
			}
		}
		if desc == "" {
			desc = fallback
		}
		id := strings.ToLower(m[1] + ":" + m[2])
		if desc != "" {
			fmt.Fprintf(&b, "%s %s\n", id, desc)
		} else {
			fmt.Fprintln(&b, id)
		}
	}
	return b.String()
}

// parseIOReg turns `ioreg -p IOUSB -l` blocks ("+-o <name>@..." then decimal idVendor/idProduct) into "vid:pid name" lines.
func parseIOReg(out string) string {
	var b strings.Builder
	for _, blk := range strings.Split(out, "+-o ")[1:] {
		v, p := macVID.FindStringSubmatch(blk), macPID.FindStringSubmatch(blk)
		if v == nil || p == nil {
			continue
		}
		vi, _ := strconv.Atoi(v[1])
		pi, _ := strconv.Atoi(p[1])
		name, _, _ := strings.Cut(blk, "@")
		fmt.Fprintf(&b, "%04x:%04x %s\n", vi, pi, strings.TrimSpace(name))
	}
	return b.String()
}

// diagnoseUSB explains why fastboot sees nothing when the deck is on the USB bus anyway.
func diagnoseUSB(ctx context.Context, o *options) {
	var hits []string
	for _, l := range strings.Split(o.ListUSB(ctx), "\n") {
		if isDeckUSB(l) {
			hits = append(hits, strings.TrimSpace(l))
		}
	}
	if len(hits) == 0 {
		return
	}
	dev := strings.Join(hits, "; ")
	if IsRockchipUSBLoader(dev) {
		o.hint("usb", "碟机在 USB 上（"+dev+"），但处于 Rockchip Loader（rkdeveloptool）模式而不是 fastboot：请重启碟机，改用 Settings → Update 里的 Firmware Update Mode。",
			"The deck is on USB ("+dev+") in the Rockchip Loader (rkdeveloptool) mode, not fastboot: reboot it and pick Firmware Update Mode under Settings → Update.")
		return
	}
	// USB enumeration does not prove the fastboot protocol is available (#129, #164).
	zh, en := "碟机已通过 USB 连接（"+dev+"），但 fastboot 还看不到它（USB 识别不代表 fastboot 协议可用）。如果碟机屏幕还在主界面，请点 Settings → Update → Firmware Update Mode (Reboot)，USB 线保持连接；",
		"The deck is on USB ("+dev+") but fastboot cannot see it yet (USB enumeration does not prove fastboot is available). If the deck still shows its main screen, tap Settings → Update → Firmware Update Mode (Reboot) and keep the cable in;"
	switch {
	case goos == "windows":
		zh += "如果已处于 Firmware Update Mode，请检查 fastboot 与驱动设置：原生 Windows 下 Google fastboot 要求设备注册 Android 设备接口 GUID（{F72FE0D4-CBCB-407D-8814-9ED673D0DD6B}）。在设备管理器中选择更新接口安装「Android Device / Android Bootloader Interface」驱动，或使用 Zadig（https://zadig.akeo.ie）选择对应的碟机更新接口（如 Denon DJ SC LIVE 4 Update 或 Numark MIXSTREAM PRO PLUS），勾选编辑接口 GUID 设为上述值安装 WinUSB。更推荐在 WSL 中运行 Linux 安装包并用 usbipd-win 直通 USB（WSL 下安装 fastboot 与 e2fsprogs，且须以 root 身份运行如 wsl -u root，避免 usbipd 设备节点权限不足导致 fastboot 识别为空）。"
		en += " if it already shows Firmware Update Mode, check fastboot and driver configuration: native Windows Google fastboot requires the device to expose the Android device interface GUID ({F72FE0D4-CBCB-407D-8814-9ED673D0DD6B}). In Device Manager, install the 'Android Device / Android Bootloader Interface' driver for the update interface, or use Zadig (https://zadig.akeo.ie) to select the deck update interface (e.g. Denon DJ SC LIVE 4 Update or Numark MIXSTREAM PRO PLUS) and configure the device interface GUID to that value. Alternatively, running the Linux package in WSL with usbipd-win is recommended (install fastboot and e2fsprogs in WSL, and run as root via 'wsl -u root' to ensure usbipd-forwarded USB device nodes are accessible to fastboot)."
	case goos == "linux" && geteuid() != 0:
		zh += "如果碟机屏幕已经处于 Firmware Update Mode，请用 sudo 运行本工具，或添加 udev 规则：\n  " + udevHint
		en += " if it already shows Firmware Update Mode, run this tool with sudo or add a udev rule:\n  " + udevHint
	default:
		zh += "如果碟机屏幕已经处于 Firmware Update Mode，请检查电脑上的 fastboot 是否能用（fastboot devices）。"
		en += " if it already shows Firmware Update Mode, check that fastboot works on this computer (fastboot devices)."
	}
	o.hint("usb", zh, en)
}

// firewallHint: decks announce themselves by UDP broadcast to port 51337 or 11224, which host firewalls
// (ufw, Windows Defender) drop by default (#129, #164).
func firewallHint(o *options) {
	var how string
	switch goos {
	case "windows":
		how = `netsh advfirewall firewall add rule name="EngineX StageLinQ" dir=in action=allow protocol=UDP localport=51337 && netsh advfirewall firewall add rule name="EngineX EAAS" dir=in action=allow protocol=UDP localport=11224`
	case "darwin":
		how = "System Settings → Network → Firewall: allow enginex-installer"
	default:
		how = "sudo ufw allow 51337/udp && sudo ufw allow 11224/udp   (firewalld: sudo firewall-cmd --add-port=51337/udp --add-port=11224/udp)"
	}
	o.say("局域网里没有碟机应答。如果电脑开着防火墙，请放行 UDP 51337（StageLinQ）与 11224（EAAS）后重试：\n  "+how+"\n  原厂未解锁的二代机（如 Mixstream Pro+、SC LIVE 系列）也可以直接进入 Firmware Update Mode 并用 USB 线连接后再运行本工具。",
		"No deck answered on the LAN. If this computer runs a firewall, allow UDP 51337 (StageLinQ) and 11224 (EAAS) and try again:\n  "+how+"\n  A stock 2nd-gen deck (e.g. Mixstream Pro+, SC LIVE) can also be booted into Firmware Update Mode and connected via USB before running this tool.")
}

// modeAB: a stock deck is on the LAN but has no SSH. Ask the user to put it in fastboot and wait.
func modeAB(ctx context.Context, o *options, d deck) error {
	name := d.Name
	if name == "" {
		name = "EngineX"
	}
	where := d.IP
	if where == "" {
		where = "USB"
	}
	label := fmt.Sprintf("%s (%s)", name, where)
	// AZ04 decks have no fastboot: their Firmware Update Mode is 1cc5:2027 (#116).
	if b := boardCode(d.Board); !haveFastboot(o) && b != "rmz2" && b != "jp24" {
		return fmt.Errorf("%s: %w", label, fastbootMissing())
	}
	model := ""
	if m, _, ok := lookupBoard(d.Board); ok {
		model = " [" + m.Name + "]"
	}
	if d.FromEAAS && d.Board == "" {
		o.say("收到来自 "+d.IP+" 的 EAAS 广播，尚不能确认设备型号。如果这是你的碟机，请在碟机上进入 Firmware Update Mode 并连接 USB；将通过 fastboot 确认型号。",
			"Received an unverified EAAS beacon from "+d.IP+"; its model is unknown. If this is your deck, enter Firmware Update Mode on the deck and connect USB; fastboot must identify the model.")
	} else {
		o.say(fmt.Sprintf("已自动发现你的碟机 %s%s，当前处于原厂未解锁状态。\n  请在碟机屏幕点击 Settings → About / Update → Firmware Update Mode (Reboot)，并用 USB 线连接电脑。程序正在自动等待 USB 连接…", label, model),
			fmt.Sprintf("Found your deck %s%s, still locked. On the deck tap Settings → About / Update → Firmware Update Mode (Reboot), then connect USB. Waiting for fastboot...", label, model))
	}
	deadline := time.Now().Add(o.WaitUSB)
	for {
		if len(fastbootDevices(ctx, o)) > 0 {
			return modeB(ctx, o)
		}
		if l := azUSB(ctx, o); l != "" {
			return modeAZ(ctx, o, d.Board, l)
		}
		diagnoseUSB(ctx, o)
		if time.Now().After(deadline) {
			return errors.New("等待 USB Fastboot 超时 / timed out waiting for fastboot")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

var (
	productRE  = regexp.MustCompile(`(?m)^(?:\(bootloader\) )?product:\s*(\S+)`)
	partSizeRE = regexp.MustCompile(`partition-size:\w+:\s*(0x[0-9a-fA-F]+|\d+)`)
)

// dataPartition is the partition Engine's /data lives on: "data" if it has a size, else "userdata".
func dataPartition(ctx context.Context, o *options) (string, int64) {
	for _, name := range []string{"data", "userdata"} {
		out, _ := fb(ctx, o, "getvar", "partition-size:"+name)
		if m := partSizeRE.FindStringSubmatch(out); m != nil {
			if n, err := strconv.ParseInt(m[1], 0, 64); err == nil && n > 0 {
				return name, n
			}
		}
	}
	return "userdata", 0
}

// JC11S stock 5.3.0 mounts the GPT entry named data, UUID
// 931ad49d-ad59-0849-833a-9bf00af5b60e. Its U-Boot exposes size/type,
// but not partition UUID. Never fall back to an unrelated userdata entry.
func jc11sDataPartition(ctx context.Context, o *options) (int64, error) {
	out, err := fb(ctx, o, "getvar", "partition-size:data")
	m := regexp.MustCompile(`(?m)^(?:\(bootloader\) )?partition-size:data:\s*(0x[0-9a-fA-F]+|\d+)\s*$`).FindStringSubmatch(out)
	var size int64
	if err == nil && m != nil {
		size, _ = strconv.ParseInt(m[1], 0, 64)
	}
	if size <= 0 {
		return 0, errors.New("JC11S: cannot verify the stock data partition size; refusing userdata fallback, nothing flashed")
	}
	out, err = fb(ctx, o, "getvar", "partition-type:data")
	if err != nil || !regexp.MustCompile(`(?m)^(?:\(bootloader\) )?partition-type:data:\s*ext4\s*$`).MatchString(out) {
		return 0, errors.New("JC11S: expected partition-type:data: ext4; target not verified, nothing flashed")
	}
	return size, nil
}

// debugfs can return success after failed writes. Check the activation chain,
// including the legacy path ALSA loads, even with an older bundle's injector.
func verifyJC11SImage(ctx context.Context, img string, b *bundleInfo, size int64) error {
	fi, err := os.Stat(img)
	if err != nil {
		return err
	}
	if fi.Size() > size {
		return errors.New("JC11S: image exceeds the verified data partition; nothing flashed")
	}
	// A real legacy directory is retained by the injector until first boot
	// migrates it. Older bundles also install only to sc5000-custom (#103).
	root := ""
	for _, path := range []string{"/enginex", "/sc5000-custom"} {
		out, err := exec.CommandContext(ctx, "debugfs", "-R", "stat "+path, img).Output()
		if err != nil {
			return fmt.Errorf("JC11S: cannot read image with debugfs: %w; nothing flashed", err)
		}
		if strings.Contains(string(out), "Type: directory") {
			root = path
			break
		}
	}
	if root == "" {
		return errors.New("JC11S: no EngineX installation directory after injection; nothing flashed")
	}
	for _, path := range []string{root + "/version.txt", root + "/bundle/version.txt"} {
		out, err := exec.CommandContext(ctx, "debugfs", "-R", "cat "+path, img).Output()
		if err != nil || strings.TrimSpace(string(out)) != b.Version {
			return fmt.Errorf("JC11S: %s does not read back as %s; nothing flashed", path, b.Version)
		}
	}
	tarData := b.TarGz
	if len(tarData) == 0 {
		raw, err := os.ReadFile(b.Path)
		if err != nil {
			return err
		}
		tarData, err = unwrapGen2ImageOrBundle(raw)
		if err != nil {
			return err
		}
	}
	gz, err := gzip.NewReader(bytes.NewReader(tarData))
	if err != nil {
		return err
	}
	defer gz.Close()
	want := activationFiles(root)
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(h.Name, "./"), b.Top+"/")
		paths, ok := want[rel]
		if !ok {
			continue
		}
		if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeRegA || h.Size <= 0 || h.Size > 32<<20 {
			return fmt.Errorf("JC11S: invalid activation file %s; nothing flashed", rel)
		}
		expected, err := io.ReadAll(tr)
		if err != nil {
			return err
		}
		for _, path := range paths {
			out, err := exec.CommandContext(ctx, "debugfs", "-R", "cat "+path, img).Output()
			if err != nil || !bytes.Equal(out, expected) {
				return fmt.Errorf("JC11S: activation file %s differs after injection; nothing flashed", path)
			}
		}
		delete(want, rel)
		for _, name := range alsaConfNames { // one ALSA file, under either name
			if rel == name {
				for _, other := range alsaConfNames {
					delete(want, other)
				}
			}
		}
	}
	if len(want) != 0 {
		return errors.New("JC11S: bundle lacks activation files; download a current PRIME4PLUS data bundle, nothing flashed")
	}
	return nil
}

// alsaConfNames are the names of the ALSA file that starts EngineX at boot: this
// release's, and the one a release after 5.3.0.4 gives it. A bundle has one of them.
var alsaConfNames = []string{"99-sc5000-custom.conf", "99-enginex.conf"}

// activationFiles maps the bundle files that start EngineX at boot to the paths
// they must have in the data image.
func activationFiles(root string) map[string][]string {
	want := map[string][]string{
		"boot-activate.sh":         {root + "/bundle/boot-activate.sh", "/sc5000-custom/bundle/boot-activate.sh"},
		"lib/libengine-ui-hook.so": {root + "/bundle/lib/libengine-ui-hook.so", "/sc5000-custom/bundle/lib/libengine-ui-hook.so"},
	}
	for _, name := range alsaConfNames {
		want[name] = []string{"/system/var/overlay/lib/alsa/conf.d/" + name}
	}
	return want
}

func python() string {
	for _, p := range []string{"python3", "python"} {
		if path, err := exec.LookPath(p); err == nil {
			return path
		}
	}
	return ""
}

// buildImage returns the /data image to flash and whether it is brand new (so empty of the user's files).
func buildImage(ctx context.Context, o *options, b *bundleInfo, size int64, name, label string) (string, bool, error) {
	final := filepath.Join(o.WorkDir, name)
	// Build and inject into a .tmp file; a half-written or stale image is never left under the real name.
	img := final + ".tmp"
	fresh := false
	ok := false
	defer func() {
		if !ok {
			os.Remove(img)
		}
	}()
	switch {
	case o.Backup != "":
		if err := copyFile(o.Backup, img); err != nil {
			return "", false, err
		}
	default:
		if size <= 0 {
			return "", false, errors.New("读不到 data 分区大小，请用 --userdata-backup / cannot read the data partition size; pass --userdata-backup")
		}
		if _, err := exec.LookPath("mke2fs"); err != nil {
			return "", false, errors.New("制作二代机 data 镜像需要 e2fsprogs (mke2fs, debugfs) 与 Python 3：Windows 请在 WSL 中以 root 运行 Linux 安装包（wsl -u root）并安装 fastboot 与 e2fsprogs，通过 usbipd-win 直通 USB；或在 Linux / macOS 电脑上运行本工具 / " +
				"building a 2nd-gen data image requires e2fsprogs (mke2fs, debugfs) and Python 3: on Windows run the Linux package in WSL as root (wsl -u root) with fastboot and e2fsprogs installed, forwarding USB via usbipd-win, or run this tool on Linux or macOS")
		}
		f, err := os.Create(img)
		if err != nil {
			return "", false, err
		}
		if err = f.Truncate(size); err == nil {
			err = f.Close()
		}
		if err != nil {
			return "", false, err
		}
		if out, err := exec.CommandContext(ctx, "mke2fs", "-t", "ext4", "-O", "encrypt", "-L", label, "-b", "4096", "-F", img).CombinedOutput(); err != nil {
			return "", false, fmt.Errorf("mke2fs: %w\n%s", err, out)
		}
		fresh = true
	}
	script := filepath.Join(mustTemp(), "host-install-userdata.py")
	if err := os.WriteFile(script, b.HostScript, 0o700); err != nil {
		return "", false, err
	}
	defer os.RemoveAll(filepath.Dir(script))
	py := python()
	if py == "" {
		return "", false, errors.New("需要 python3 和 e2fsprogs (debugfs, mke2fs)；Windows 请在 WSL 里运行 / needs python3 and e2fsprogs; on Windows use WSL")
	}
	bundleArg := b.Path
	if b.IsImage && len(b.TarGz) > 0 {
		bundleArg = filepath.Join(filepath.Dir(script), b.Top+".tar.gz")
		if err := os.WriteFile(bundleArg, b.TarGz, 0o600); err != nil {
			return "", false, err
		}
	}
	args := []string{"-I", script, img, "--bundle", bundleArg}
	if pub := sshPublicKey(o); pub != "" && bytes.Contains(b.HostScript, []byte("--ssh-key")) { // older bundles do not know the flag
		args = append(args, "--ssh-key", pub)
	}
	if out, err := exec.CommandContext(ctx, py, args...).CombinedOutput(); err != nil {
		return "", false, fmt.Errorf("host-install-userdata: %w\n%s", err, out)
	}
	if err := os.Rename(img, final); err != nil {
		return "", false, err
	}
	ok = true
	return final, fresh, nil
}

// sshPublicKey is the .pub of --key, else of the first ~/.ssh/id_* that has one; "" when there is none.
func sshPublicKey(o *options) string {
	if o.Key != "" {
		if p := o.Key + ".pub"; fileExists(p) {
			return p
		}
		return ""
	}
	home, _ := os.UserHomeDir()
	for _, n := range []string{"id_ed25519", "id_rsa", "id_ecdsa"} {
		if p := filepath.Join(home, ".ssh", n+".pub"); fileExists(p) {
			return p
		}
	}
	return ""
}

func mustTemp() string {
	d, err := os.MkdirTemp("", "enginex-installer-")
	if err != nil {
		panic(err)
	}
	return d
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err = io.Copy(out, in); err == nil {
		err = out.Close()
	}
	return err
}

func confirm(o *options, zh, en string) error {
	if o.Yes {
		return nil
	}
	o.say(zh+" 输入 yes 继续：", en+" Type yes to continue:")
	line, _ := bufio.NewReader(o.In).ReadString('\n')
	if s := strings.ToLower(strings.TrimSpace(line)); s != "yes" && s != "y" {
		return errors.New("已取消，没有刷写任何东西 / cancelled, nothing flashed")
	}
	return nil
}

func modeB(ctx context.Context, o *options) error {
	out, _ := fb(ctx, o, "getvar", "product")
	board := ""
	if pm := productRE.FindStringSubmatch(out); pm != nil {
		board = pm[1]
	} else if board, _ = usbDeck(ctx, o, gen1USB); board == "" {
		return fmt.Errorf("读不到 fastboot product / cannot read the fastboot product: %s", strings.TrimSpace(out))
	}
	m, gen2, ok := lookupBoard(board)
	switch {
	case !ok:
		return fmt.Errorf("未知型号 %q，不刷写 / unknown board, nothing flashed", board)
	}
	o.say("USB Fastboot 识别到 "+m.Name+" ("+board+")", "Fastboot found "+m.Name)
	if !gen2 {
		return modeBGen1(ctx, o, m)
	}
	b, err := obtainBundle(ctx, o, m.Prefix)
	if err != nil {
		return err
	}
	part, size := "data", int64(0)
	if boardCode(board) == "jc11s" {
		size, err = jc11sDataPartition(ctx, o)
		if err != nil {
			return err
		}
	} else {
		part, size = dataPartition(ctx, o)
	}
	img, fresh, err := buildImage(ctx, o, b, size, "userdata-enginex.img", "az01-data")
	if err != nil {
		return err
	}
	if boardCode(board) == "jc11s" {
		if err := verifyJC11SImage(ctx, img, b, size); err != nil {
			return err
		}
	}
	if fresh {
		err = confirm(o, "即将刷写全新的 "+part+" 分区：碟机上的音乐、曲库和设置会全部清空！", "This flashes a brand-new "+part+" partition: ALL music, library and settings on the deck are erased!")
	} else {
		err = confirm(o, "即将把 "+img+" 刷入 "+part+" 分区，必须是这台碟机的 data 备份。", "This flashes "+img+" to "+part+"; it must be a backup of this deck's data.")
	}
	if err != nil {
		return err
	}
	sayGen2Steps(o, filepath.Base(b.Path))
	var sizeStr string
	if fi, err := os.Stat(img); err == nil && fi.Size() > 0 {
		sizeStr = " (" + formatBytes(fi.Size()) + ")"
	}
	o.say(fmt.Sprintf("正在向碟机写入 %s 分区%s，通过 USB 传输可能需要几分钟，请勿断开连接或关闭电源…", part, sizeStr),
		fmt.Sprintf("Flashing %s partition%s over USB; this may take several minutes. Do not disconnect USB or power off the deck...", part, sizeStr))
	if out, err := fbLive(ctx, o, "flash", part, img); err != nil {
		return fmt.Errorf("fastboot flash %s: %w\n%s", part, err, out)
	}
	o.say("正在重启碟机…", "Rebooting deck...")
	if out, err := fb(ctx, o, "reboot"); err != nil {
		return fmt.Errorf("fastboot reboot: %w\n%s", err, out)
	}
	if boardCode(board) == "jc11s" {
		imgPattern := fmt.Sprintf("%s-%s-EngineX-OS-*-Update.img", m.Prefix, b.Version)
		o.say(
			fmt.Sprintf("USB 写入完成（已写入 gen2-unlocked 解锁标记）；重启后的激活尚未验证。请在 Settings → About 确认显示 EngineX OS 并检查 EngineX 页面：激活成功后，今后升级无需电脑，可直接通过 U 盘或 Settings -> About / Update 刷入 %s。若仍为原厂 5.3.0，请保留日志并报告 #169；不要重复清空 data 或恢复出厂。", imgPattern),
			fmt.Sprintf("USB write complete; activation after reboot is not verified. Confirm EngineX OS in Settings → About and open the EngineX page: once active, future updates can flash %s directly from a USB drive or on-deck Settings -> About / Update without a computer. If it still shows stock 5.3.0, keep this log and report #169; do not repeatedly erase data or factory-reset.", imgPattern),
		)
		return nil
	}
	o.say(fmt.Sprintf("完成：EngineX OS %s 已刷入 %s，碟机正在重启，首次启动后自动激活。", b.Version, m.Name),
		fmt.Sprintf("Done: EngineX OS %s flashed to %s; the deck reboots and activates itself.", b.Version, m.Name))
	sayGen2Unlocked(o, b, m.Prefix)
	return nil
}

func sayGen2Steps(o *options, filename string) {
	o.say("[1/2] 正在一键解锁二代机 .img 升级链路（gen2-unlocked + 更新器预加载）…",
		"[1/2] Unlocking 2nd-generation .img update chain (gen2-unlocked + updater preload)...")
	o.say("[2/2] 正在刷入 "+filename+"…",
		"[2/2] Flashing "+filename+"...")
}

// sayGen2Unlocked tells the user that the 2nd-generation deck is unlocked and flashed (#119, #215).
func sayGen2Unlocked(o *options, b *bundleInfo, prefix string) {
	if !bytes.Contains(b.HostScript, []byte("gen2-unlocked")) {
		return
	}
	imgPattern := fmt.Sprintf("%s-%s-EngineX-OS-*-Update.img", prefix, b.Version)
	o.say(
		fmt.Sprintf("碟机已完成一键解锁与刷入：今后升级无需电脑，可直接把 %s 拷入 U 盘插上碟机升级，或在碟机屏幕点击 Settings -> About / Update 在线更新；恢复出厂设置不会丢失 EngineX OS。", imgPattern),
		fmt.Sprintf("Deck is unlocked and flashed: future updates can flash %s directly from a USB drive or on-deck Settings -> About / Update without a computer, and Factory Reset keeps EngineX OS.", imgPattern),
	)
}

func modeBGen1(ctx context.Context, o *options, m model) error {
	path, err := fetchImage(ctx, o, m.Prefix)
	if err != nil {
		return err
	}
	py := python()
	if py == "" {
		return errors.New("Python 3 required to unpack firmware")
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	packer := o.Packer
	if packer == "" {
		packer = filepath.Join(filepath.Dir(exe), "az01_packer.py")
	}
	if !fileExists(packer) {
		return errors.New("az01_packer.py missing beside installer")
	}
	tmp, err := os.MkdirTemp("", "enginex-rootfs-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	out, err := exec.CommandContext(ctx, py, "-I", packer, "unpack", path, "--output", filepath.Join(tmp, "unpacked")).CombinedOutput()
	if err != nil {
		return fmt.Errorf("unpack: %w\n%s", err, out)
	}
	rootfs := filepath.Join(tmp, "unpacked", "rootfs.ext2")
	if !fileExists(rootfs) {
		return errors.New("unpacked rootfs missing")
	}
	if err := confirm(o, "将解锁并刷写 "+m.Name+" 的 rootfs，可能损坏设备或数据，请先备份。", "Unlock and flash "+m.Name+" rootfs; device or data could be damaged. Back up first."); err != nil {
		return err
	}
	o.say("正在解锁碟机…", "Unlocking deck...")
	if out, err := fb(ctx, o, "oem", "inmusic-unlock-magic-7de5fbc22b8c524e"); err != nil {
		return fmt.Errorf("fastboot oem unlock: %w\n%s", err, out)
	}
	var sizeStr string
	if fi, err := os.Stat(rootfs); err == nil && fi.Size() > 0 {
		sizeStr = " (" + formatBytes(fi.Size()) + ")"
	}
	o.say(fmt.Sprintf("正在向碟机写入 rootfs 分区%s，通过 USB 传输可能需要几分钟，请勿断开连接或关闭电源…", sizeStr),
		fmt.Sprintf("Flashing rootfs partition%s over USB; this may take several minutes. Do not disconnect USB or power off the deck...", sizeStr))
	if out, err := fbLive(ctx, o, "flash", "rootfs", rootfs); err != nil {
		return fmt.Errorf("fastboot flash rootfs: %w\n%s", err, out)
	}
	o.say("正在重启碟机…", "Rebooting deck...")
	if out, err := fb(ctx, o, "reboot"); err != nil {
		return fmt.Errorf("fastboot reboot: %w\n%s", err, out)
	}
	return nil
}

// ---- mode C: Firmware Update Mode without fastboot (1cc5:2027) --------------

const (
	azID       = "1cc5:2027"
	azDataSize = 64 << 20 // the fresh /data image SBDJUK flashed on a SYSTEM ONE (#116)
)

// azNames match the USB product string when nothing else names the deck; most specific first.
var azNames = []struct{ name, board string }{
	{"system one", "rmz2"}, {"prime 4 g2", "jp24"}, {"prime 4+", "jc11s"}, {"prime 4 plus", "jc11s"},
	{"sc live 4", "jp21"}, {"sc live 2", "jp20"}, {"prime go+", "jp11s"}, {"mixstream pro+", "nh08s"}, {"mixstream pro go", "nh10"},
}

// azUSB returns the host USB line of a deck in 1cc5:2027 Firmware Update Mode (az0x-splash usbupdate), or "".
func azUSB(ctx context.Context, o *options) string {
	for _, l := range strings.Split(o.ListUSB(ctx), "\n") {
		if l = strings.TrimSpace(l); strings.Contains(strings.ToLower(l), azID) {
			return l
		}
	}
	return ""
}

// azModel: the board seen on the LAN (if an AZ04 deck without fastboot), else the --bundle file name, else the USB product name.
func azModel(o *options, board, line string) (model, bool) {
	// Only AZ04 decks without fastboot (rmz2 SYSTEM ONE, jp24 PRIME 4 G2) use 1cc5:2027 write-part.
	// A fastboot deck (e.g. nh08s, jp21) discovered on the LAN must never be passed to modeAZ (#164).
	if b := boardCode(board); b == "rmz2" || b == "jp24" {
		if m, gen2, ok := lookupBoard(board); ok && gen2 {
			return m, true
		}
	}
	if o.Bundle != "" {
		if m, ok := bundleModel(o.Bundle); ok {
			return m, true
		}
	}
	l := strings.ToLower(line)
	for _, n := range azNames {
		if strings.Contains(l, n.name) {
			return gen2Boards[n.board], true
		}
	}
	// Last resort: exactly one 2nd-gen model's *-Update.img or Data-Bundle.tar.gz already in the work directory.
	if o.Bundle == "" && board == "" {
		var matched model
		seen := map[string]bool{}
		entries, _ := os.ReadDir(o.WorkDir)
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			n := e.Name()
			if m, ok := bundleModel(n); ok && (isGen2UpdateImgName(n, m.Prefix) || isGen2BundleName(n, m.Prefix)) {
				matched = m
				seen[m.Prefix] = true
			}
		}
		if len(seen) == 1 {
			return matched, true
		}
	}
	return model{}, false
}

func bundleModel(path string) (model, bool) {
	base := filepath.Base(path)
	for _, m := range gen2Boards {
		if strings.HasPrefix(base, m.Prefix+"-") {
			return m, true
		}
	}
	return model{}, false
}

func modeAZ(ctx context.Context, o *options, board, line string) error {
	m, ok := azModel(o, board, line)
	if !ok {
		return fmt.Errorf("碟机处于 Firmware Update Mode（%s），但认不出型号：请用 --image 或 --bundle 指定该机型的 *-Update.img，没有刷写任何东西 / "+
			"the deck is in Firmware Update Mode (%s) but its model is unknown: pass --image or --bundle <MODEL>-<VERSION>-EngineX-OS-*-Update.img; nothing flashed", line, line)
	}
	o.say("USB Firmware Update Mode（"+azID+"）识别到 "+m.Name, "Firmware Update Mode ("+azID+") found "+m.Name)
	b, err := obtainBundle(ctx, o, m.Prefix)
	if err != nil {
		return err
	}
	if b.USBScript == nil {
		return errors.New(filepath.Base(b.Path) + " 太旧，没有 host-install-usbupdate.py，请下载最新版 / is too old to flash over " + azID + ": get the current release")
	}
	img, fresh, err := buildImage(ctx, o, b, azDataSize, "data-enginex.img", "data")
	if err != nil {
		return err
	}
	if fresh {
		err = confirm(o, "即将刷写全新的 data 分区：碟机内置存储上的音乐、曲库和设置会全部清空（外接 U 盘/SD 卡不受影响）！",
			"This flashes a brand-new data partition: ALL music, library and settings on the deck's internal storage are erased (USB drives and SD cards are not)!")
	} else {
		err = confirm(o, "即将把 "+img+" 刷入 data 分区，必须是这台碟机的 data 备份。", "This flashes "+img+" to data; it must be a backup of this deck's data.")
	}
	if err != nil {
		return err
	}
	sayGen2Steps(o, filepath.Base(b.Path))
	o.say("正在通过 USB 写入 data 分区，请勿断开连接或关闭电源…", "Writing the data partition over USB. Do not disconnect USB or power off the deck...")
	if err := o.USBFlash(ctx, o, b.USBScript, img); err != nil {
		return fmt.Errorf("USB %s write-part: %w", azID, err)
	}
	// 1cc5:2027 has no reboot command over USB.
	o.say(fmt.Sprintf("完成：EngineX OS %s 已写入 %s。请按电源键关机再开机（或拔插电源），开机后自动激活。", b.Version, m.Name),
		fmt.Sprintf("Done: EngineX OS %s written to %s. Press the power button to turn it off and on again (or replug power); it activates on boot.", b.Version, m.Name))
	sayGen2Unlocked(o, b, m.Prefix)
	return nil
}

// usbFlash runs the bundle's host-install-usbupdate.py (pyusb) with its progress on o.Out.
func usbFlash(ctx context.Context, o *options, script []byte, img string) error {
	py := python()
	if py == "" {
		return errors.New("需要 python3 和 pyusb / needs python3 with pyusb (pip3 install pyusb)")
	}
	dir := mustTemp()
	defer os.RemoveAll(dir)
	p := filepath.Join(dir, "host-install-usbupdate.py")
	if err := os.WriteFile(p, script, 0o700); err != nil {
		return err
	}
	// -E, not -I: pyusb is often a user-site install, which -I hides. The script's dir is ours alone.
	cmd := exec.CommandContext(ctx, py, "-E", p, img)
	cmd.Stdout, cmd.Stderr = o.Out, o.Out
	return cmd.Run()
}

// ---- main -------------------------------------------------------------------

func run(ctx context.Context, o *options) error {
	ignoreSIGPIPE()
	o.defaults()
	o.guide()
	if !o.Uninstall {
		if len(fastbootDevices(ctx, o)) > 0 {
			return modeB(ctx, o)
		}
		if l := azUSB(ctx, o); l != "" {
			return modeAZ(ctx, o, "", l)
		}
	}
	o.say("正在局域网中寻找碟机…", "Looking for decks on the LAN...")
	var errs []error
	n := 0
	var eaas []deck
	for _, d := range discover(ctx, o) {
		if d.FromEAAS && d.Board == "" {
			eaas = append(eaas, d)
			continue
		}
		if !d.Known && !d.FromEAAS {
			o.say("跳过非碟机的 StageLinQ 设备 "+d.Name+" ("+d.IP+")", "Skipping StageLinQ device "+d.Name+" ("+d.IP+")")
			continue
		}
		n++
		if err := modeA(ctx, o, d); err != nil {
			errs = append(errs, err)
		}
	}
	if n == 0 {
		if o.Uninstall {
			return errors.New("局域网中未发现可卸载的碟机：--uninstall 需碟机开机、接入同一网络并开启 SSH / no deck found on the LAN to uninstall: --uninstall requires the deck to be powered on, on the same network, with SSH accessible")
		}
		if board, line := usbDeck(ctx, o, gen2USB); board != "" { // a stock 2nd-gen deck on USB, in any mode: wait for it to reboot into fastboot
			return modeAB(ctx, o, deck{Name: line, Board: board})
		}
		// Every unverified candidate shares the same host USB bus. Wait once,
		// rather than giving each unauthenticated IP its own full USB timeout.
		if len(eaas) > 0 {
			return modeAB(ctx, o, eaas[0])
		}
		diagnoseUSB(ctx, o)
		firewallHint(o)
		o.guide()
		return errors.New("没有发现碟机：请开机并接入同一局域网，或把碟机进入 Firmware Update Mode 后用 USB 连接 / no deck found: power it on in this LAN, or boot it into Firmware Update Mode and connect USB")
	}
	return errors.Join(errs...)
}

func registerFlags(fs *flag.FlagSet, o *options) {
	fs.StringVar(&o.Bundle, "bundle", "", "*-Update.img or Data-Bundle.tar.gz to install (default: newest in the current directory, else download)")
	fs.StringVar(&o.Bundle, "image", "", "alias for -bundle: *-Update.img to install")
	fs.StringVar(&o.Backup, "userdata-backup", "", "mode B: a backup image of the deck's data partition to inject into (keeps the user's files; if omitted, a fresh partition is created)")
	fs.StringVar(&o.Key, "key", "", "mode A: SSH private key (default ~/.ssh/id_*; password denon is also tried)")
	fs.StringVar(&o.Password, "password", "", "mode A: SSH password for root")
	fs.StringVar(&o.Repo, "repo", "Cd1s/EngineX-OS", "GitHub repository to download from")
	fs.BoolVar(&o.Yes, "yes", false, "do not ask before flashing")
	fs.BoolVar(&o.Uninstall, "uninstall", false, "remove the EngineX data bundle from a 2nd-generation deck over SSH and return to stock")
}

func main() {
	o := &options{}
	registerFlags(flag.CommandLine, o)
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	err := run(ctx, o)
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误 / Error:", err)
	}
	if st, e := os.Stdin.Stat(); e == nil && st.Mode()&os.ModeCharDevice != 0 && !o.Yes { // double-clicked: keep the window
		fmt.Print("\n按回车退出 / Press Enter to exit")
		bufio.NewReader(os.Stdin).ReadString('\n')
	}
	if err != nil {
		os.Exit(1)
	}
}
