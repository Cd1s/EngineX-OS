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
	// in deemix-config.json (via http://sc5000.lan:8080) or SC5000_STEMS_URL.
	DefaultRelayURL = ""
	// DefaultRelayConfig is stream-sync's relay settings file on the deck.
	DefaultRelayConfig = "/data/sc5000-custom/state/deemix-config.json"
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
//  1. SC5000_STEMS_URL set: talk to that address directly. The credential is
//     SC5000_STEMS_TOKEN, else the token file (SC5000_STEMS_TOKEN_FILE,
//     default /etc/sc5000-stems/token).
//  2. Otherwise the relay from deemix-config.json (SC5000_STEMS_CONFIG,
//     default /data/sc5000-custom/state/deemix-config.json): relayUrl and the
//     console password deemixPassword. SC5000_STEMS_TOKEN overrides the
//     password.
func ResolveClient(getenv func(string) string) (Client, error) {
	override := strings.TrimSpace(getenv("SC5000_STEMS_TOKEN"))
	if endpoint := strings.TrimSpace(getenv("SC5000_STEMS_URL")); endpoint != "" {
		if !validEndpoint(endpoint) {
			return Client{}, errors.New("SC5000_STEMS_URL must be https, or http on a private network")
		}
		if override != "" {
			return Client{endpoint, override, "SC5000_STEMS_URL"}, nil
		}
		path := getenv("SC5000_STEMS_TOKEN_FILE")
		if path == "" {
			path = DefaultTokenFile
		}
		secret, err := Secret(path)
		if err != nil {
			return Client{}, fmt.Errorf("token for SC5000_STEMS_URL: %w", err)
		}
		return Client{endpoint, secret, "SC5000_STEMS_URL"}, nil
	}
	path := getenv("SC5000_STEMS_CONFIG")
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

// readRelayConfig reads the fields stream-sync and sc5000-deemixd share.
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

// validEndpoint keeps the password off the clear-text internet, matching the
// deck console's rule for the relay address (plus tailnet CGNAT addresses).
func validEndpoint(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	if u.Scheme != "http" {
		return false
	}
	host := u.Hostname()
	if ip := net.ParseIP(host); ip != nil {
		return lanIP(ip)
	}
	lh := strings.ToLower(strings.TrimSuffix(host, "."))
	if lh == "" || strings.HasPrefix(lh, ".") || strings.Contains(lh, "..") {
		return false
	}
	return !strings.Contains(lh, ".") || strings.HasSuffix(lh, ".local") || strings.HasSuffix(lh, ".lan")
}

var cgnat = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}
