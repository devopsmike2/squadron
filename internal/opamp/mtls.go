// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package opamp

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
)

// SetOpAMPTLS wires the ADR 0052 slice-2 mTLS seam: it installs the TLS config
// the OpAMP control-channel listener serves with. nil (the OSS default, and
// every test harness) leaves the listener PLAINTEXT (ws://) — behavior is
// unchanged. The enterprise edition builds a BYO-CA mutual-TLS config (see
// BuildMutualTLSConfig) from operator-supplied cert/key/CA material and installs
// it here, so the port requires and verifies a client certificate (wss:// +
// mTLS). Call once at startup before Start; not safe for concurrent use with a
// running server.
//
// BYO-CA (ADR 0052): Squadron only VERIFIES against the operator's CA — it does
// not issue, rotate, or revoke certificates. The operator distributes per-agent
// client certs out of band (their existing PKI), which fits air-gapped fleets.
func (s *Server) SetOpAMPTLS(cfg *tls.Config) { s.tlsConfig = cfg }

// BuildMutualTLSConfig builds a bring-your-own-CA mutual-TLS config for the
// OpAMP listener from PEM material: the server's own cert + key (presented to
// agents) and the CA bundle used to REQUIRE AND VERIFY each agent's client
// certificate. It is pure (no I/O, no globals) and reused by the enterprise wire
// that reads the operator's configured paths; OSS ships it unused (mTLS is the
// enterprise wedge, ADR 0042/0001).
//
// The returned config sets ClientAuth = RequireAndVerifyClientCert (an agent
// with no cert, or a cert not chaining to clientCAPEM, is rejected at the TLS
// handshake — before any OpAMP message is processed) and MinVersion TLS 1.2.
func BuildMutualTLSConfig(serverCertPEM, serverKeyPEM, clientCAPEM []byte) (*tls.Config, error) {
	if len(serverCertPEM) == 0 || len(serverKeyPEM) == 0 {
		return nil, errors.New("mtls: server cert and key PEM are both required")
	}
	if len(clientCAPEM) == 0 {
		return nil, errors.New("mtls: client CA PEM is required (BYO-CA: the CA that signs agent client certs)")
	}
	serverCert, err := tls.X509KeyPair(serverCertPEM, serverKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("mtls: load server keypair: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(clientCAPEM) {
		return nil, errors.New("mtls: client CA PEM contained no valid certificates")
	}
	return &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS12,
	}, nil
}
