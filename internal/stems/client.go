package stems

// Where stems-processor sends audio. By default the deck reuses the cloud
// relay address and password it already keeps for logins, so a new install
// needs no extra address, port or key file. Environment variables override it.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
)

const (
	// DefaultRelayURL is empty: operators configure their own self-hosted relay
	// in deemix-config.json (via http://enginex.lan:8080) or ENGINEX_STEMS_URL.
	DefaultRelayURL = ""
	// DefaultRelayConfig is stream-sync's relay settings file on the deck.
	DefaultRelayConfig = "/data/enginex/state/deemix-config.json"
	// DefaultTokenFile holds the renderer's internal token for direct mode.
	DefaultTokenFile = "/etc/sc5000-stems/token"
)

// Client is a resolved backend: the base URL Proxy posts /v1/engine to and
// the bearer credential it presents. An empty Endpoint means no renderer is
// set up (a standalone deck): stems-processor then renders locally.
type Client struct {
	Endpoint string
	Secret   string
	// Source names where the settings came from, for error messages. It never
	// contains the credential.
	Source string
}

// ResolveClient picks the backend in this order:
//
//  1. ENGINEX_STEMS_URL set: talk to that address directly. The credential is
//     ENGINEX_STEMS_TOKEN, else the token file (ENGINEX_STEMS_TOKEN_FILE,
//     default /etc/sc5000-stems/token).
//  2. Otherwise the relay from deemix-config.json (ENGINEX_STEMS_CONFIG,
//     default /data/enginex/state/deemix-config.json): relayUrl and the
//     console password deemixPassword. ENGINEX_STEMS_TOKEN overrides the
//     password.
//
// Each ENGINEX_STEMS_* name is also read as SC5000_STEMS_*, its name before 5.3.0.5 (#234).
func ResolveClient(getenv func(string) string) (Client, error) {
	getenv = withLegacyStemsEnv(getenv)
	override := strings.TrimSpace(getenv("ENGINEX_STEMS_TOKEN"))
	if endpoint := strings.TrimSpace(getenv("ENGINEX_STEMS_URL")); endpoint != "" {
		if !validEndpoint(endpoint) {
			return Client{}, errors.New("ENGINEX_STEMS_URL must be https, or http on a private network")
		}
		if override != "" {
			return Client{endpoint, override, "ENGINEX_STEMS_URL"}, nil
		}
		path := getenv("ENGINEX_STEMS_TOKEN_FILE")
		if path == "" {
			path = DefaultTokenFile
		}
		secret, err := Secret(path)
		if err != nil {
			return Client{}, fmt.Errorf("token for ENGINEX_STEMS_URL: %w", err)
		}
		return Client{endpoint, secret, "ENGINEX_STEMS_URL"}, nil
	}
	path := getenv("ENGINEX_STEMS_CONFIG")
	if path == "" {
		path = DefaultRelayConfig
	}
	relay, password, err := readRelayConfig(path)
	if errors.Is(err, os.ErrNotExist) {
		return Client{Source: "standalone"}, nil
	}
	if err != nil {
		return Client{}, err
	}
	if override != "" {
		password = override
	}
	if password == "" || relay == "" {
		// No relay: stems-processor looks for a home renderer, then renders locally.
		return Client{Source: "standalone"}, nil
	}
	if !validEndpoint(relay) {
		return Client{}, fmt.Errorf("%s: relay address must be https, or http on a private network", path)
	}
	return Client{relay, password, path}, nil
}

// readRelayConfig reads the fields stream-sync and enginex-deemixd share.
func readRelayConfig(path string) (relay, password string, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", "", fmt.Errorf("relay settings: %w", err)
	}
	var v struct {
		RelayURL  string `json:"relayUrl"`
		DeemixURL string `json:"deemixUrl"` // older sc5000-deemixd name
		Password  string `json:"deemixPassword"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return "", "", fmt.Errorf("%s: invalid JSON", path)
	}
	relay = strings.TrimSpace(v.RelayURL)
	if relay == "" {
		relay = strings.TrimSpace(v.DeemixURL)
	}
	if relay == "" {
		relay = DefaultRelayURL
	}
	return relay, strings.TrimSpace(v.Password), nil
}

// ValidEndpoint keeps credentials off the clear-text internet, shared by both
// stems.ResolveClient / ProbeLAN and the deck console's relay URL check.
func ValidEndpoint(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	host := u.Hostname()
	ipHost, zone, hasZone := strings.Cut(host, "%")
	if hasZone && zone == "" {
		return false
	}
	if u.Scheme == "https" {
		if strings.Contains(host, ":") {
			ip := net.ParseIP(ipHost)
			return ip != nil && (!hasZone || ip.To4() == nil)
		}
		return !hasZone
	}
	if u.Scheme != "http" {
		return false
	}
	if ip := net.ParseIP(ipHost); ip != nil {
		if hasZone && ip.To4() != nil {
			return false
		}
		return lanIP(ip)
	}
	if hasZone || strings.Contains(host, ":") {
		return false
	}
	lh := strings.ToLower(strings.TrimSuffix(host, "."))
	if lh == "" || strings.HasPrefix(lh, ".") || strings.Contains(lh, "..") {
		return false
	}
	return !strings.Contains(lh, ".") || strings.HasSuffix(lh, ".local") || strings.HasSuffix(lh, ".lan")
}

func validEndpoint(raw string) bool {
	return ValidEndpoint(raw)
}

var cgnat = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

// withLegacyStemsEnv falls back to SC5000_STEMS_<x> when ENGINEX_STEMS_<x> is not set (#234).
func withLegacyStemsEnv(getenv func(string) string) func(string) string {
	return func(name string) string {
		if v := getenv(name); v != "" {
			return v
		}
		if rest, ok := strings.CutPrefix(name, "ENGINEX_STEMS_"); ok {
			return getenv("SC5000_STEMS_" + rest)
		}
		return ""
	}
}
