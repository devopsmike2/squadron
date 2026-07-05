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
	audit    AuditService // optional; nil-safe. Added in v0.51 to
	// produce api_token.issued / revoked / expired audit events for
	// CIP-007-6 R4.1.2 and R5.3 evidence.
	logger *zap.Logger
}

// NewAuthService creates an AuthService.
func NewAuthService(appStore applicationstore.ApplicationStore, logger *zap.Logger) AuthService {
	return &AuthServiceImpl{appStore: appStore, logger: logger}
}

// SetAuditService wires audit fan-out post-construction. Used by
// main.go because the auth service is built before the audit service
// in the dependency graph.
func (s *AuthServiceImpl) SetAuditService(a AuditService) {
	s.audit = a
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
//
// Scopes is required and validated against the known-scope set. The
// caller may pass [ScopeWildcard] to grant full access (still
// recorded explicitly so the audit log shows it). An empty scope list
// is rejected at the service layer — operators must opt in to the
// permissions they're granting. The "empty == legacy full access"
// behavior in APIToken.HasScope exists only for tokens persisted
// before v0.10.
func (s *AuthServiceImpl) Issue(ctx context.Context, label string, scopes []string, expiresAt *time.Time) (*APIToken, string, error) {
	stored, plaintext, err := buildToken(label, scopes, expiresAt)
	if err != nil {
		return nil, "", err
	}
	if err := s.appStore.CreateAPIToken(ctx, stored); err != nil {
		return nil, "", fmt.Errorf("failed to persist token: %w", err)
	}
	s.logger.Info("issued api token",
		zap.String("token_id", stored.ID),
		zap.String("label", stored.Label),
		zap.Strings("scopes", stored.Scopes))
	s.emitIssuedAudit(ctx, stored)
	return toServiceToken(stored), plaintext, nil
}

// IssueTx mints a token as part of a caller-owned transaction: it does the same
// validation / entropy / hashing / row-build as Issue, but runs the INSERT
// through the supplied Execer (a *sql.Tx) instead of the store's own *sql.DB, so
// the write commits or rolls back with the caller's other writes. This is the
// OSS half of the ADR-0015 transactional-mint seam — an enterprise SSO overlay
// wraps this token INSERT together with its tenant-assign + RBAC-bind writes in
// ONE tx on its shared handle, so the mint is truly atomic (no revoke-on-failure
// compensation, no orphaned token).
//
// IMPORTANT: IssueTx deliberately does NOT emit the api_token.issued audit event.
// The write may still roll back after IssueTx returns (if a later step in the
// caller's tx fails), and auditing a token that never persisted would be untrue.
// The caller emits the issued audit AFTER it commits; EmitIssuedAudit exposes the
// canonical event shape for that. Inert in OSS: nothing here calls IssueTx.
//
// It is intentionally NOT part of the AuthService interface (so OSS test fakes
// and any alternate implementation are unaffected); the enterprise overlay
// reaches it by type-assertion, exactly as main.go reaches SetAuditService.
func (s *AuthServiceImpl) IssueTx(ctx context.Context, exec applicationstore.Execer, label string, scopes []string, expiresAt *time.Time) (*APIToken, string, error) {
	if exec == nil {
		return nil, "", fmt.Errorf("IssueTx: nil execer")
	}
	txStore, ok := s.appStore.(interface {
		CreateAPITokenTx(ctx context.Context, exec applicationstore.Execer, t *applicationstore.APIToken) error
	})
	if !ok {
		return nil, "", fmt.Errorf("IssueTx: application store does not support transactional token creation")
	}
	stored, plaintext, err := buildToken(label, scopes, expiresAt)
	if err != nil {
		return nil, "", err
	}
	if err := txStore.CreateAPITokenTx(ctx, exec, stored); err != nil {
		return nil, "", fmt.Errorf("failed to persist token: %w", err)
	}
	s.logger.Info("issued api token (tx)",
		zap.String("token_id", stored.ID),
		zap.String("label", stored.Label),
		zap.Strings("scopes", stored.Scopes))
	return toServiceToken(stored), plaintext, nil
}

// EmitIssuedAudit records the canonical api_token.issued audit event for a token
// that was minted via IssueTx, to be called by the caller AFTER its transaction
// commits (IssueTx itself does not emit — see its doc). Nil-safe: no-op when no
// audit service is wired. tok is the service-layer token returned by IssueTx.
func (s *AuthServiceImpl) EmitIssuedAudit(ctx context.Context, tok *APIToken) {
	if tok == nil {
		return
	}
	stored := &applicationstore.APIToken{
		ID:        tok.ID,
		Label:     tok.Label,
		Scopes:    tok.Scopes,
		ExpiresAt: tok.ExpiresAt,
	}
	s.emitIssuedAudit(ctx, stored)
}

// emitIssuedAudit fans out the api_token.issued audit event (v0.51, CIP-007-6
// R5.3 / SOC 2 CC6.2). Nil-safe. Shared by Issue (inline) and EmitIssuedAudit
// (post-commit for the IssueTx path) so the event payload is defined once.
func (s *AuthServiceImpl) emitIssuedAudit(ctx context.Context, stored *applicationstore.APIToken) {
	if s.audit == nil {
		return
	}
	payload := map[string]any{
		"label":  stored.Label,
		"scopes": stored.Scopes,
	}
	if stored.ExpiresAt != nil {
		payload["expires_at"] = stored.ExpiresAt.Format(time.RFC3339)
	}
	_ = s.audit.Record(ctx, AuditEntry{
		EventType:  "api_token.issued",
		TargetType: "api_token",
		TargetID:   stored.ID,
		Action:     "issued",
		Payload:    payload,
	})
}

// buildToken performs the label/scope/expiry validation, generates the token
// entropy, and builds the stored row + plaintext. Shared by Issue and IssueTx so
// the two mint paths validate and hash identically. It does NOT touch the store
// or audit — the caller persists (with or without a tx) and audits.
func buildToken(label string, scopes []string, expiresAt *time.Time) (*applicationstore.APIToken, string, error) {
	label = strings.TrimSpace(label)
	if len(label) < labelMinLen {
		return nil, "", fmt.Errorf("label is required")
	}
	if len(label) > labelMaxLen {
		return nil, "", fmt.Errorf("label must be %d chars or fewer", labelMaxLen)
	}
	if len(scopes) == 0 {
		return nil, "", fmt.Errorf("scopes is required: pass [\"*\"] for full access or a specific list")
	}
	dedup := make(map[string]struct{}, len(scopes))
	normalized := make([]string, 0, len(scopes))
	for _, sc := range scopes {
		sc = strings.TrimSpace(sc)
		if sc == "" {
			continue
		}
		if !IsValidScope(sc) {
			return nil, "", fmt.Errorf("unknown scope %q", sc)
		}
		if _, dup := dedup[sc]; dup {
			continue
		}
		dedup[sc] = struct{}{}
		normalized = append(normalized, sc)
	}
	if len(normalized) == 0 {
		return nil, "", fmt.Errorf("scopes is required: pass [\"*\"] for full access or a specific list")
	}

	// Reject expiries already in the past. Operators sometimes copy a
	// past date by accident (e.g. forgetting to bump a year); rejecting
	// here surfaces it as a clean 400 rather than a token that 401s
	// the first time it's used.
	now := time.Now().UTC()
	if expiresAt != nil {
		exp := expiresAt.UTC()
		if !exp.After(now) {
			return nil, "", fmt.Errorf("expires_at must be in the future")
		}
		expiresAt = &exp
	}

	// 32 bytes of cryptographic entropy. Base64-URL-encoded so the
	// result is filename-safe and copy-paste-safe (no slashes).
	buf := make([]byte, tokenEntropyBytes)
	if _, err := rand.Read(buf); err != nil {
		return nil, "", fmt.Errorf("failed to read entropy: %w", err)
	}
	plaintext := tokenPrefix + base64.RawURLEncoding.EncodeToString(buf)
	hash := hashToken(plaintext)

	stored := &applicationstore.APIToken{
		ID:        uuid.New().String(),
		Label:     label,
		Hash:      hash,
		Scopes:    normalized,
		CreatedAt: now,
		ExpiresAt: expiresAt,
	}
	return stored, plaintext, nil
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
	// v0.51 — emit api_token.revoked audit event for CIP-007-6 R5.3
	// (authorize access changes) and the HIPAA log-in monitoring
	// requirement. Look up the label for the payload so an auditor
	// reading the event doesn't need a second lookup.
	if s.audit != nil {
		var label string
		for _, t := range tokens {
			if t.ID == id {
				label = t.Label
				break
			}
		}
		_ = s.audit.Record(ctx, AuditEntry{
			EventType:  "api_token.revoked",
			TargetType: "api_token",
			TargetID:   id,
			Action:     "revoked",
			Payload:    map[string]any{"label": label},
		})
	}
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
	// Expired tokens hit the same 401 path as unknown / revoked. We
	// deliberately don't leak "your token is specifically expired"
	// because a guesser learning a string was once valid is itself
	// information.
	if stored.ExpiresAt != nil && !time.Now().Before(*stored.ExpiresAt) {
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
	out := &APIToken{
		ID:         t.ID,
		Label:      t.Label,
		CreatedAt:  t.CreatedAt,
		LastUsedAt: t.LastUsedAt,
		RevokedAt:  t.RevokedAt,
		ExpiresAt:  t.ExpiresAt,
		TenantID:   t.TenantID, // ADR 0011: thread tenant through to the actor
	}
	if len(t.Scopes) > 0 {
		out.Scopes = make([]string, len(t.Scopes))
		copy(out.Scopes, t.Scopes)
	}
	return out
}
