// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/storage/applicationstore"
)

// AuthServiceImpl is the canonical AuthService implementation backed by
// the application store.
type AuthServiceImpl struct {
	appStore applicationstore.ApplicationStore
	logger   *zap.Logger
}

// NewAuthService creates an AuthService.
func NewAuthService(appStore applicationstore.ApplicationStore, logger *zap.Logger) AuthService {
	return &AuthServiceImpl{appStore: appStore, logger: logger}
}

// tokenPrefix is the human-readable marker on every issued token. Makes
// it obvious what a string is when it shows up in logs / config files
// / paste buffers, and lets us cheaply reject obviously-not-a-token
// values at the middleware layer before hashing.
const tokenPrefix = "sqd_"

// tokenEntropyBytes is the random body length. 32 bytes = 256 bits,
// rendered as 43-char base64url. Total token length ~47 chars.
const tokenEntropyBytes = 32

// labelMaxLen and labelMinLen bound the operator-supplied label. Labels
// flow into audit log actors verbatim ("operator:<label>") so we keep
// them short and printable.
const (
	labelMinLen = 1
	labelMaxLen = 64
)

// Issue creates a token, persists its hash, and returns both the
// canonical row and the plaintext value. The plaintext is the ONLY
// time the operator (or anyone) can see the token — Squadron does not
// retain a recoverable copy.
func (s *AuthServiceImpl) Issue(ctx context.Context, label string) (*APIToken, string, error) {
	label = strings.TrimSpace(label)
	if len(label) < labelMinLen {
		return nil, "", fmt.Errorf("label is required")
	}
	if len(label) > labelMaxLen {
		return nil, "", fmt.Errorf("label must be %d chars or fewer", labelMaxLen)
	}

	// 32 bytes of cryptographic entropy. Base64-URL-encoded so the
	// result is filename-safe and copy-paste-safe (no slashes).
	buf := make([]byte, tokenEntropyBytes)
	if _, err := rand.Read(buf); err != nil {
		return nil, "", fmt.Errorf("failed to read entropy: %w", err)
	}
	plaintext := tokenPrefix + base64.RawURLEncoding.EncodeToString(buf)
	hash := hashToken(plaintext)

	now := time.Now().UTC()
	stored := &applicationstore.APIToken{
		ID:        uuid.New().String(),
		Label:     label,
		Hash:      hash,
		CreatedAt: now,
	}
	if err := s.appStore.CreateAPIToken(ctx, stored); err != nil {
		return nil, "", fmt.Errorf("failed to persist token: %w", err)
	}
	s.logger.Info("issued api token", zap.String("token_id", stored.ID), zap.String("label", stored.Label))
	return toServiceToken(stored), plaintext, nil
}

// List returns every token Squadron has ever issued, revoked or not,
// newest first. The plaintext is never included — see the type's
// missing field.
func (s *AuthServiceImpl) List(ctx context.Context) ([]*APIToken, error) {
	stored, err := s.appStore.ListAPITokens(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*APIToken, len(stored))
	for i, t := range stored {
		out[i] = toServiceToken(t)
	}
	return out, nil
}

// Revoke marks a token as revoked. Idempotent at the storage layer —
// re-revoking a revoked token is a no-op rather than an error. Service
// layer returns an error only if the token doesn't exist at all so the
// UI can show a clear "not found" rather than a silent success.
func (s *AuthServiceImpl) Revoke(ctx context.Context, id string) error {
	// Find the token to give a clean "not found" error. List is cheap
	// (auth tokens are bounded in practice) and the UI calls revoke
	// from a list anyway.
	tokens, err := s.appStore.ListAPITokens(ctx)
	if err != nil {
		return err
	}
	found := false
	for _, t := range tokens {
		if t.ID == id {
			found = true
			break
		}
	}
	if !found {
		return errors.New("token not found")
	}
	if err := s.appStore.RevokeAPIToken(ctx, id, time.Now().UTC()); err != nil {
		return err
	}
	s.logger.Info("revoked api token", zap.String("token_id", id))
	return nil
}

// Validate hashes the plaintext and looks up the matching row.
// Returns (token, nil) if a non-revoked token matches; (nil, nil) for
// "unknown or revoked"; (nil, err) for storage failure. The middleware
// treats unknown and revoked identically (both → 401) so the response
// doesn't leak whether a guessed token "almost worked".
//
// Best-effort updates last_used_at; failure is logged at Warn but does
// not fail the validation.
func (s *AuthServiceImpl) Validate(ctx context.Context, plaintext string) (*APIToken, error) {
	if !strings.HasPrefix(plaintext, tokenPrefix) {
		return nil, nil
	}
	hash := hashToken(plaintext)
	stored, err := s.appStore.GetAPITokenByHash(ctx, hash)
	if err != nil {
		return nil, err
	}
	if stored == nil || stored.RevokedAt != nil {
		return nil, nil
	}
	if err := s.appStore.UpdateAPITokenLastUsed(ctx, stored.ID, time.Now().UTC()); err != nil {
		s.logger.Warn("failed to update token last_used_at",
			zap.String("token_id", stored.ID), zap.Error(err))
	}
	return toServiceToken(stored), nil
}

// hashToken is the canonical sha256 hex digest function used at both
// Issue and Validate time. Hex (not base64) so the column is fixed
// width and easy to inspect by hand if needed.
func hashToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

func toServiceToken(t *applicationstore.APIToken) *APIToken {
	if t == nil {
		return nil
	}
	return &APIToken{
		ID:         t.ID,
		Label:      t.Label,
		CreatedAt:  t.CreatedAt,
		LastUsedAt: t.LastUsedAt,
		RevokedAt:  t.RevokedAt,
	}
}
