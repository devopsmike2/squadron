// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package opamp

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"go.uber.org/zap"
)

// selfSignedPEM returns a throwaway self-signed cert + key (PEM) usable both as a
// server keypair and, reused, as a client CA bundle for the tests.
func selfSignedPEM(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "squadron-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey: %v", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}

func TestBuildMutualTLSConfig_HappyPath(t *testing.T) {
	certPEM, keyPEM := selfSignedPEM(t)
	caPEM, _ := selfSignedPEM(t)

	cfg, err := BuildMutualTLSConfig(certPEM, keyPEM, caPEM)
	if err != nil {
		t.Fatalf("BuildMutualTLSConfig: %v", err)
	}
	if cfg.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Errorf("ClientAuth = %v, want RequireAndVerifyClientCert (mTLS must reject uncerted/untrusted agents)", cfg.ClientAuth)
	}
	if cfg.ClientCAs == nil {
		t.Error("ClientCAs pool must be set from the BYO-CA bundle")
	}
	if len(cfg.Certificates) != 1 {
		t.Errorf("Certificates = %d, want 1 server keypair", len(cfg.Certificates))
	}
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %d, want TLS1.2+", cfg.MinVersion)
	}
}

func TestBuildMutualTLSConfig_Errors(t *testing.T) {
	certPEM, keyPEM := selfSignedPEM(t)
	caPEM, _ := selfSignedPEM(t)

	cases := []struct {
		name              string
		cert, key, ca     []byte
	}{
		{"no server cert", nil, keyPEM, caPEM},
		{"no server key", certPEM, nil, caPEM},
		{"no client CA", certPEM, keyPEM, nil},
		{"bad server keypair", []byte("not a pem"), []byte("nope"), caPEM},
		{"bad CA pem", certPEM, keyPEM, []byte("-----BEGIN CERTIFICATE-----\nbad\n-----END CERTIFICATE-----")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := BuildMutualTLSConfig(tc.cert, tc.key, tc.ca); err == nil {
				t.Errorf("%s: expected an error, got nil", tc.name)
			}
		})
	}
}

func TestSetOpAMPTLS(t *testing.T) {
	s := &Server{logger: zap.NewNop()}
	if s.tlsConfig != nil {
		t.Fatal("default tlsConfig should be nil (OSS plaintext)")
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS13}
	s.SetOpAMPTLS(cfg)
	if s.tlsConfig != cfg {
		t.Error("SetOpAMPTLS should install the provided config")
	}
}
