package webui

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultHTTPSPort = 8443
	httpsCertFile    = "console-cert.pem"
	httpsKeyFile     = "console-key.pem"
)

// tlsPersistDir stores the deck's generated self-signed certificate across boots.
// Overridden in TestMain so tests never write to the build host's /data/enginex.
var tlsPersistDir = "/data/enginex"

type closeOnceListener struct {
	net.Listener
	once sync.Once
	err  error
}

func (l *closeOnceListener) Close() error {
	l.once.Do(func() {
		l.err = l.Listener.Close()
	})
	return l.err
}

func (s *Server) tlsDir() string {
	if s.cfg.TLSCertDir != "" {
		return s.cfg.TLSCertDir
	}
	if st, err := os.Stat(tlsPersistDir); err == nil && st.IsDir() {
		return tlsPersistDir
	}
	if s.cfg.StateDir != "" {
		return s.cfg.StateDir
	}
	return s.uiDir()
}

func collectCertSANs() ([]string, []net.IP) {
	var dnsNames []string
	seenDNS := map[string]bool{}
	addDNS := func(name string) {
		name = strings.TrimSpace(strings.TrimSuffix(name, "."))
		if name == "" || net.ParseIP(name) != nil || seenDNS[name] {
			return
		}
		seenDNS[name] = true
		dnsNames = append(dnsNames, name)
	}

	var ips []net.IP
	seenIP := map[string]bool{}
	addIP := func(ip net.IP) {
		if ip == nil || ip.IsUnspecified() {
			return
		}
		if v4 := ip.To4(); v4 != nil {
			ip = v4
		}
		k := ip.String()
		if seenIP[k] {
			return
		}
		seenIP[k] = true
		ips = append(ips, ip)
	}

	addDNS("localhost")
	if h, err := os.Hostname(); err == nil && h != "" {
		if ip := net.ParseIP(h); ip != nil {
			addIP(ip)
		} else {
			addDNS(h)
			if !strings.Contains(h, ".") {
				addDNS(h + ".local")
			}
		}
	}
	addDNS("enginex.local")
	addDNS("sc5000.lan")

	addIP(net.ParseIP("127.0.0.1"))
	addIP(net.ParseIP("::1"))
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok {
				addIP(n.IP)
			}
		}
	}
	return dnsNames, ips
}

func writeSecretFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".tls-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func loadOrCreateConsoleCert(dir string) (tls.Certificate, error) {
	certPath := filepath.Join(dir, httpsCertFile)
	keyPath := filepath.Join(dir, httpsKeyFile)
	if cert, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil && len(cert.Certificate) > 0 {
		if leaf, err := x509.ParseCertificate(cert.Certificate[0]); err == nil {
			if time.Now().Before(leaf.NotAfter) {
				return cert, nil
			}
		}
	}

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return tls.Certificate{}, err
	}
	dnsNames, ipAddrs := collectCertSANs()
	cn := "EngineX OS"
	if h, err := os.Hostname(); err == nil && strings.TrimSpace(h) != "" {
		cn = strings.TrimSpace(h)
	}
	now := time.Now()
	notBefore := now.Add(-24 * time.Hour)
	if notBefore.Year() < 2025 {
		notBefore = time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	notAfter := now.AddDate(10, 0, 0)
	if minEnd := time.Date(2036, 1, 1, 0, 0, 0, 0, time.UTC); notAfter.Before(minEnd) {
		notAfter = minEnd
	}
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              dnsNames,
		IPAddresses:           ipAddrs,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return tls.Certificate{}, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	if err := writeSecretFile(keyPath, keyPEM); err != nil {
		return tls.Certificate{}, err
	}
	if err := writeSecretFile(certPath, certPEM); err != nil {
		return tls.Certificate{}, err
	}
	return tls.X509KeyPair(certPEM, keyPEM)
}

func (s *Server) applyHTTPS(on bool) error {
	if s.cfg.HTTPSAddr == "" {
		return nil
	}
	if !on {
		s.stopHTTPS(false)
		return nil
	}
	return s.startHTTPS()
}

func (s *Server) startHTTPS() error {
	s.httpsMu.Lock()
	defer s.httpsMu.Unlock()
	if s.httpsSrv != nil {
		return nil
	}
	cert, err := loadOrCreateConsoleCert(s.tlsDir())
	if err != nil {
		return err
	}
	rawLn, err := net.Listen("tcp", s.cfg.HTTPSAddr)
	if err != nil {
		return err
	}
	ln := &closeOnceListener{Listener: rawLn}
	port := defaultHTTPSPort
	if addr, ok := rawLn.Addr().(*net.TCPAddr); ok && addr.Port > 0 {
		port = addr.Port
	}
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
	srv := &http.Server{
		Handler:           s,
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16384,
	}
	s.httpsSrv = srv
	s.httpsLn = ln
	s.httpsPort = port
	s.httpsWG.Add(1)
	go func() {
		defer s.httpsWG.Done()
		if err := srv.Serve(tls.NewListener(ln, tlsCfg)); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			log.Printf("stream-sync: HTTPS listener %s: %v", s.cfg.HTTPSAddr, err)
		}
	}()
	return nil
}

func (s *Server) stopHTTPS(wait bool) {
	s.httpsMu.Lock()
	srv := s.httpsSrv
	ln := s.httpsLn
	s.httpsSrv = nil
	s.httpsLn = nil
	s.httpsPort = 0
	s.httpsMu.Unlock()
	if ln != nil {
		_ = ln.Close()
	}
	if srv == nil {
		return
	}
	shutdown := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}
	if wait {
		shutdown()
		return
	}
	s.httpsWG.Add(1)
	go func() {
		defer s.httpsWG.Done()
		shutdown()
	}()
}

func (s *Server) httpsActive() bool {
	s.httpsMu.Lock()
	defer s.httpsMu.Unlock()
	return s.httpsSrv != nil
}

func (s *Server) httpsPortNum() int {
	s.httpsMu.Lock()
	p := s.httpsPort
	s.httpsMu.Unlock()
	if p > 0 {
		return p
	}
	if s.cfg.HTTPSAddr != "" {
		if _, portStr, err := net.SplitHostPort(s.cfg.HTTPSAddr); err == nil {
			if n, err := strconv.Atoi(portStr); err == nil && n > 0 {
				return n
			}
		}
	}
	return defaultHTTPSPort
}

func (s *Server) httpsPortStr() string {
	return strconv.Itoa(s.httpsPortNum())
}

func (s *Server) httpsOfferedLocked(enabled bool) bool {
	return enabled && (s.cfg.HTTPSAddr == "" || s.httpsActive())
}

func (s *Server) httpsOffered() bool {
	s.uiMu.Lock()
	enabled := s.loadUI().ConsoleHTTPS
	s.uiMu.Unlock()
	return s.httpsOfferedLocked(enabled)
}
