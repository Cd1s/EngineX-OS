package webui

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

type copyBundle struct {
	ReceivedAt time.Time                  `json:"-"`
	Version    int                        `json:"version"`
	Board      string                     `json:"board"`
	Native     map[string]json.RawMessage `json:"native"`
	UI         map[string]json.RawMessage `json:"ui"`
}

type copyPeerRequest struct {
	Token   string      `json:"token"`
	Peer    string      `json:"peer"`
	Expires time.Time   `json:"-"`
	Denied  bool        `json:"-"`
	Bundle  *copyBundle `json:"-"`
}

type copyControl struct {
	ID            string                     `json:"id"`
	BundleHash    string                     `json:"bundleHash"`
	Identity      map[string]json.RawMessage `json:"identity"`
	Action        string                     `json:"action"`
	Peer          string                     `json:"peer"`
	Token         string                     `json:"token"`
	Native        map[string]json.RawMessage `json:"native"`
	Entries       map[string][]string        `json:"entries"`
	Loading       *bool                      `json:"loading"`
	Playing       *bool                      `json:"playing"`
	Confirmed     bool                       `json:"confirmed"`
	Drive         string                     `json:"drive"`
	IncludeUI     bool                       `json:"includeEngineX"`
	ProfileHash   string                     `json:"profileHash"`
	PreviousDrive string                     `json:"previousDrive"`
}

var copyPeerHTTP = &http.Client{
	Timeout:       8 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func copyRemoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return ""
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return ""
	}
	return ip.String()
}

func (s *Server) copyPeers() []string {
	s.shadow.mu.Lock()
	defer s.shadow.mu.Unlock()
	var peers []string
	for ip, seen := range s.shadow.peers {
		if net.ParseIP(ip) != nil && time.Since(seen) < 6*time.Second {
			peers = append(peers, ip)
		}
	}
	sort.Strings(peers)
	return peers
}

func (s *Server) copyKnownPeer(ip string) bool {
	for _, peer := range s.copyPeers() {
		if peer == ip {
			return true
		}
	}
	return false
}

func (s *Server) copyPeerURL(ip, path string) string {
	port := s.shadow.peerPort
	if port == 0 {
		port = 8080
	}
	return "http://" + net.JoinHostPort(ip, strconv.Itoa(port)) + path
}

func decodeCopyRequest(w http.ResponseWriter, r *http.Request, into any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(into); err != nil {
		return errors.New("invalid settings copy request")
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("invalid settings copy request")
	}
	return nil
}

func (s *Server) settingsCopyRequest(w http.ResponseWriter, r *http.Request) {
	ip := copyRemoteIP(r)
	if !s.copyKnownPeer(ip) {
		fail(w, 403, errors.New("requester is not a discovered EngineX peer"))
		return
	}
	var in struct {
		Token string `json:"token"`
	}
	if err := decodeCopyRequest(w, r, &in); err != nil || len(in.Token) != 32 {
		fail(w, 400, errors.New("invalid settings copy request"))
		return
	}
	if _, err := hex.DecodeString(in.Token); err != nil {
		fail(w, 400, errors.New("invalid settings copy request"))
		return
	}
	s.copyMu.Lock()
	defer s.copyMu.Unlock()
	if s.copyIncoming != nil && time.Now().Before(s.copyIncoming.Expires) {
		fail(w, 409, errors.New("another settings request is waiting"))
		return
	}
	// Metadata only. No settings are read until the source screen's Allow.
	s.copyIncoming = &copyPeerRequest{Token: in.Token, Peer: ip, Expires: time.Now().Add(time.Minute)}
	respond(w, map[string]any{"waiting": true})
}

func (s *Server) settingsCopyTransfer(w http.ResponseWriter, r *http.Request) {
	s.copyMu.Lock()
	defer s.copyMu.Unlock()
	p := s.copyIncoming
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "SettingsCopy ")
	if p == nil || !time.Now().Before(p.Expires) || p.Peer != copyRemoteIP(r) ||
		subtle.ConstantTimeCompare([]byte(token), []byte(p.Token)) != 1 {
		fail(w, 401, errors.New("settings transfer grant required"))
		return
	}
	if p.Denied {
		fail(w, 403, errors.New("source declined settings copy"))
		return
	}
	if p.Bundle == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	bundle := p.Bundle
	s.copyIncoming = nil // consumed even if the requester disconnects
	respond(w, bundle)
}

func (s *Server) copyBegin(ip string) error {
	if !s.copyKnownPeer(ip) {
		return errors.New("source is not a discovered EngineX peer")
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	p := &copyPeerRequest{Token: hex.EncodeToString(nonce[:]), Peer: ip, Expires: time.Now().Add(time.Minute)}
	b, _ := json.Marshal(map[string]string{"token": p.Token})
	response, err := copyPeerHTTP.Post(s.copyPeerURL(ip, "/peer/settings-copy/request"), "application/json", bytes.NewReader(b))
	if err != nil {
		return errors.New("could not reach source deck")
	}
	response.Body.Close()
	if response.StatusCode != 200 {
		return errors.New("source deck cannot accept this request")
	}
	s.copyMu.Lock()
	s.copyOutgoing, s.copyReceived = p, nil
	s.copyMu.Unlock()
	return nil
}

func (s *Server) copyAllow(r *http.Request, in copyControl) error {
	if !copyLocalScreen(r) {
		return errors.New("Allow must be confirmed on the source deck screen")
	}
	// The source's local UI already receives its own access code via /v1/ui.
	// It authenticates here; that credential never enters the transfer payload.
	if ok, _ := s.tryCode(r, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")); !ok {
		return errors.New("source screen authentication required")
	}
	s.copyMu.Lock()
	defer s.copyMu.Unlock()
	p := s.copyIncoming
	if p == nil || !time.Now().Before(p.Expires) || in.Token != p.Token || p.Bundle != nil || p.Denied {
		return errors.New("settings request expired")
	}
	if in.Action == "deny" {
		p.Denied = true
		return nil
	}
	if len(in.Native) == 0 {
		return errors.New("native preferences are not ready")
	}
	for key, raw := range in.Native {
		if err := validateCopyNative(key, raw); err != nil {
			return err
		}
	}
	s.uiMu.Lock()
	u := copyUIExport(s.loadUI())
	s.uiMu.Unlock()
	p.Bundle = &copyBundle{Version: 1, Board: imagePrefix(deviceCompatible()), Native: in.Native, UI: u}
	return nil
}

func (s *Server) copyPoll() error {
	s.copyMu.Lock()
	p := s.copyOutgoing
	if p == nil || !time.Now().Before(p.Expires) {
		s.copyMu.Unlock()
		return errors.New("settings request expired")
	}
	peer, token := p.Peer, p.Token
	s.copyMu.Unlock()
	keepWaiting := false
	defer func() {
		if keepWaiting {
			return
		}
		s.copyMu.Lock()
		if s.copyOutgoing != nil && s.copyOutgoing.Token == token {
			s.copyOutgoing = nil
		}
		s.copyMu.Unlock()
	}()
	req, _ := http.NewRequest(http.MethodGet, s.copyPeerURL(peer, "/api/settings-copy/transfer"), nil)
	req.Header.Set("Authorization", "SettingsCopy "+token)
	response, err := copyPeerHTTP.Do(req)
	if err != nil {
		return errors.New("could not reach source deck")
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusAccepted {
		keepWaiting = true
		return nil
	}
	if response.StatusCode != 200 {
		return fmt.Errorf("source declined or the settings request expired (%d)", response.StatusCode)
	}
	var bundle copyBundle
	d := json.NewDecoder(io.LimitReader(response.Body, (1<<20)+1))
	d.DisallowUnknownFields()
	if err := d.Decode(&bundle); err != nil || bundle.Version != 1 {
		return errors.New("invalid settings bundle")
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("invalid settings bundle")
	}
	for key, raw := range bundle.Native {
		if err := validateCopyNative(key, raw); err != nil {
			return err
		}
	}
	s.uiMu.Lock()
	_, _, err = copyUIMerge(s.loadUI(), bundle.UI, imagePrefix(deviceCompatible()))
	s.uiMu.Unlock()
	if err != nil {
		return err
	}
	s.copyMu.Lock()
	if s.copyOutgoing != nil && s.copyOutgoing.Token == token {
		bundle.ReceivedAt = time.Now()
		s.copyReceived, s.copyOutgoing = &bundle, nil
	}
	s.copyMu.Unlock()
	return nil
}
