// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package oci

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// SigningKey is the fully unsealed material the scanner uses to sign
// OCI REST API requests per the OCI HTTP Signatures spec (RFC 7616
// superset). The struct bundles the three OCID-shaped identifier
// fields with the parsed RSA private key so callers don't pass four
// arguments to SignRequest at every call site.
//
// Slice 1 only signs GET requests; the simpler signature surface
// (request-target + date + host) is enough. POST/PUT/DELETE in slice
// 2+ will add the content-type / content-length / x-content-sha256
// headers per the spec.
//
// The PrivateKey field is the parsed RSA key — callers obtain it via
// ParsePrivateKey, which decodes the PEM bytes the credstore's
// UnsealOCIPrivateKey returned. The plaintext PEM bytes NEVER appear
// in error strings, log lines, audit payloads, or HTTP responses
// (substrate invariant inherited from the seal/unseal pair); the
// SigningKey wrapper is the only sanctioned in-memory home for the
// parsed key.
type SigningKey struct {
	TenancyOCID string
	UserOCID    string
	Fingerprint string
	PrivateKey  *rsa.PrivateKey
}

// ParsePrivateKey decodes a PEM-encoded RSA private key. Returns a
// parse error if the key is malformed (no PEM header, wrong type,
// PKCS#1 / PKCS#8 parse failure). The error message NEVER carries
// the key bytes.
//
// Supports both PKCS#1 ("RSA PRIVATE KEY") and PKCS#8 ("PRIVATE
// KEY") PEM blocks — the OCI tooling emits PKCS#1 by default
// (openssl genrsa) but PKCS#8 is increasingly common (openssl
// genpkey, openssl pkcs8). The slice 1 wizard step pastes whatever
// the operator generated; supporting both shapes keeps the wizard
// friction-free.
func ParsePrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("oci: private key PEM decode failed (no PEM block found)")
	}
	switch block.Type {
	case "RSA PRIVATE KEY":
		k, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("oci: PKCS1 private key parse failed: %w", err)
		}
		return k, nil
	case "PRIVATE KEY":
		k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("oci: PKCS8 private key parse failed: %w", err)
		}
		rsaKey, ok := k.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("oci: PKCS8 key is not RSA (got %T)", k)
		}
		return rsaKey, nil
	default:
		return nil, fmt.Errorf("oci: unsupported PEM block type %q (expected RSA PRIVATE KEY or PRIVATE KEY)", block.Type)
	}
}

// KeyID returns the OCI keyId format used in the Signature header:
//
//	"<tenancy_ocid>/<user_ocid>/<fingerprint>"
//
// OCI's gateway resolves this triple to the registered public key
// it should use to verify the signature.
func (sk *SigningKey) KeyID() string {
	return sk.TenancyOCID + "/" + sk.UserOCID + "/" + sk.Fingerprint
}

// SignRequest signs an HTTP GET request per OCI HTTP Signatures
// spec. Sets Authorization, Date, and Host headers on req. Returns
// an error only if the RSA-SHA256 sign call itself fails (which is
// effectively never in production — RSA signing is deterministic
// and bounded once the key is parsed).
//
// The signing string for a GET request is:
//
//	(request-target): get /<path>?<query>
//	date: <RFC1123 date in UTC>
//	host: <host>
//
// Each line is separated by \n with no trailing newline. The
// resulting Authorization header has the form:
//
//	Signature version="1",keyId="<tenancy>/<user>/<fingerprint>",
//	  algorithm="rsa-sha256",
//	  headers="(request-target) date host",
//	  signature="<base64_sig>"
//
// (rendered on a single line in the actual header — the line breaks
// here are illustrative).
//
// POST/PUT/DELETE in slice 2+ will extend the headers to include
// content-type, content-length, and x-content-sha256 per the spec.
func (sk *SigningKey) SignRequest(req *http.Request) error {
	if sk == nil || sk.PrivateKey == nil {
		return errors.New("oci: SignRequest: SigningKey has no parsed RSA private key")
	}
	if req == nil {
		return errors.New("oci: SignRequest: request is nil")
	}

	// Date header (RFC 1123 in UTC — OCI's gateway is strict about
	// the exact spec). http.TimeFormat is RFC 1123 with GMT, which
	// matches.
	date := time.Now().UTC().Format(http.TimeFormat)
	req.Header.Set("Date", date)

	// Host header — OCI verifies this matches the URL's host.
	host := req.URL.Host
	req.Header.Set("Host", host)

	// Build the request-target line: "get <path>?<query>" all
	// lowercase method, percent-encoded path as the URL carries it.
	method := strings.ToLower(req.Method)
	target := req.URL.RequestURI() // path + ?query

	signingString := strings.Join([]string{
		"(request-target): " + method + " " + target,
		"date: " + date,
		"host: " + host,
	}, "\n")

	hashed := sha256.Sum256([]byte(signingString))
	sig, err := rsa.SignPKCS1v15(rand.Reader, sk.PrivateKey, crypto.SHA256, hashed[:])
	if err != nil {
		// In practice RSA signing on a parsed key with valid hash
		// input cannot fail; the error path exists for completeness.
		return fmt.Errorf("oci: RSA-SHA256 sign failed: %w", err)
	}
	sigB64 := base64.StdEncoding.EncodeToString(sig)

	authz := fmt.Sprintf(
		`Signature version="1",keyId=%q,algorithm="rsa-sha256",headers="(request-target) date host",signature=%q`,
		sk.KeyID(),
		sigB64,
	)
	req.Header.Set("Authorization", authz)
	return nil
}
