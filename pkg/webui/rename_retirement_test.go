package webui

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cd1s/EngineX-OS/internal/migration"
)

func TestRename234ReissuesLegacyCertificateOnce(t *testing.T) {
	dir := t.TempDir()
	cert, err := loadOrCreateConsoleCert(dir)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	leaf.DNSNames = append(leaf.DNSNames, migration.PlaceholderHost)
	key := cert.PrivateKey.(crypto.Signer)
	der, err := x509.CreateCertificate(rand.Reader, leaf, leaf, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeSecretFile(filepath.Join(dir, httpsCertFile), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})); err != nil {
		t.Fatal(err)
	}
	beforeKey, _ := os.ReadFile(filepath.Join(dir, httpsKeyFile))
	migrated, err := loadOrCreateConsoleCert(dir)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err = x509.ParseCertificate(migrated.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range leaf.DNSNames {
		if name == migration.PlaceholderHost {
			t.Fatal("legacy SAN remains")
		}
	}
	afterKey, _ := os.ReadFile(filepath.Join(dir, httpsKeyFile))
	if !bytes.Equal(beforeKey, afterKey) {
		t.Fatal("private key rotated")
	}
	beforeCert, _ := os.ReadFile(filepath.Join(dir, httpsCertFile))
	if _, err := loadOrCreateConsoleCert(dir); err != nil {
		t.Fatal(err)
	}
	afterCert, _ := os.ReadFile(filepath.Join(dir, httpsCertFile))
	if !bytes.Equal(beforeCert, afterCert) {
		t.Fatal("current certificate was regenerated")
	}
}

func TestRename234RetiredAPIAndOutputs(t *testing.T) {
	s := New(Config{StateDir: t.TempDir(), UIDir: t.TempDir()}, nil)
	defer s.Close()
	for _, method := range []string{"GET", "POST"} {
		t.Run(method, func(t *testing.T) {
			r := httptest.NewRequest(method, migration.APIPrefix+"/ui", nil)
			r.RemoteAddr = "127.0.0.1:1234"
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			if w.Code != 404 {
				t.Errorf("retired namespace returned %d", w.Code)
			}
		})
	}
	old := uiScaleEnvPath
	uiScaleEnvPath = filepath.Join(t.TempDir(), "scale.env")
	defer func() { uiScaleEnvPath = old }()
	if err := writeUIScaleEnv(75); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(uiScaleEnvPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "ENGINEX_UI_SCALE=0.75\n" {
		t.Errorf("unexpected scale output %q", b)
	}
	names, _ := collectCertSANs()
	if strings.Contains(strings.Join(names, " "), migration.PlaceholderHost) {
		t.Error("retired certificate domain emitted")
	}
}
