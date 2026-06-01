package stream

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/kubexa/kubexa-agent/pkg/config"
)

// transportCredentials builds gRPC transport credentials from gateway settings.
// TLS is enabled by default; insecure credentials are used only when TLS is disabled.
func transportCredentials(gw *config.GatewayConfig) (credentials.TransportCredentials, error) {
	if gw == nil {
		return nil, fmt.Errorf("gateway config is nil")
	}
	if !gw.TLS {
		return insecure.NewCredentials(), nil
	}

	tlsCfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
	}

	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}

	if gw.CACertPath != "" {
		pem, err := os.ReadFile(gw.CACertPath)
		if err != nil {
			return nil, fmt.Errorf("read gateway CA cert %q: %w", gw.CACertPath, err)
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("parse gateway CA cert %q: no certificates found", gw.CACertPath)
		}
	}

	tlsCfg.RootCAs = pool
	return credentials.NewTLS(tlsCfg), nil
}
