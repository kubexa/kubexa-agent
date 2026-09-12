package stream

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/kubexa/kubexa-agent/pkg/config"
)

func writeSelfSignedCA(t *testing.T, path string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A transient construction failure -- the CA file not yet mounted on the
// first console -- must not be remembered for the process lifetime: the
// next dial retries the construction. Nothing listens on the address, so
// a dial that got past construction fails with the NETWORK's error; one
// that did not fails with the CA file's.
func TestExecDialerRetriesAFailedConstruction(t *testing.T) {
	ca := filepath.Join(t.TempDir(), "ca.pem")
	cfg := &config.Config{}
	cfg.Gateway.Address = "127.0.0.1:1"
	cfg.Gateway.TLS = true
	cfg.Gateway.CACertPath = ca
	dial := NewExecDialer(cfg, nil)

	_, err := dial(context.Background())
	if err == nil || !strings.Contains(err.Error(), "gateway CA cert") {
		t.Fatalf("first dial with the CA file missing: %v, want the CA read error", err)
	}
	writeSelfSignedCA(t, ca)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = dial(ctx)
	if err == nil {
		t.Fatal("dial succeeded although nothing listens")
	}
	if strings.Contains(err.Error(), "gateway CA cert") {
		t.Fatalf("second dial: %v -- the first construction failure was remembered", err)
	}
	if status.Code(err) != codes.Unavailable && status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("second dial: %v, want the network's own refusal", err)
	}
}
