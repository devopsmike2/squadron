// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/devopsmike2/squadron/internal/siem"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// SiemService is the service-layer interface for managing SIEM
// destinations and feeding them to the dispatcher. It owns the
// crypter and is the only thing in the codebase that holds
// plaintext secrets in memory; the API layer never sees them.
//
// Added in v0.50.2 alongside the storage layer from v0.50.1.
type SiemService interface {
	Create(ctx context.Context, input SiemDestinationInput) (*SiemDestinationView, error)
	Get(ctx context.Context, id string) (*SiemDestinationView, error)
	List(ctx context.Context) ([]*SiemDestinationView, error)
	Update(ctx context.Context, id string, input SiemDestinationUpdate) (*SiemDestinationView, error)
	Delete(ctx context.Context, id string) error

	// Test sends a synthetic event to the destination and returns
	// the dispatcher result. Lets operators verify the URL + secret
	// + reachability before the SIEM team starts depending on
	// audit data flowing through.
	Test(ctx context.Context, id, actor string) error

	// LoadEnabled is the siem.SourceProvider hook the dispatcher
	// polls every reload tick. Decrypts each destination's secret
	// just before handing it back; the dispatcher uses the secret
	// only to build the exporter and drops the reference after.
	LoadEnabled(ctx context.Context) ([]*siem.Destination, [][]byte, error)
}

// SiemDestinationInput is the body shape Create accepts.
//
// PlaintextSecret never leaves the service layer in plaintext form:
// it's encrypted with the crypter and stored as ciphertext. After
// Create returns the SiemDestinationView, callers cannot retrieve
// the plaintext back — they have to re-issue.
type SiemDestinationInput struct {
	Name            string   `json:"name"`
	Type            string   `json:"type"` // splunk_hec | webhook
	URL             string   `json:"url"`
	PlaintextSecret string   `json:"plaintext_secret"`
	Enabled         bool     `json:"enabled"`
	EventTypePrefix []string `json:"event_type_prefix,omitempty"`
}

// SiemDestinationUpdate is the body shape Update accepts. All
// fields are pointers so PUT semantics can distinguish "leave it
// alone" from "set to zero value". PlaintextSecret nil means "keep
// the existing ciphertext"; a non-nil empty string is rejected
// (operators wanting to remove the secret should delete and
// recreate the destination).
type SiemDestinationUpdate struct {
	Name            *string   `json:"name"`
	Type            *string   `json:"type"`
	URL             *string   `json:"url"`
	PlaintextSecret *string   `json:"plaintext_secret"`
	Enabled         *bool     `json:"enabled"`
	EventTypePrefix *[]string `json:"event_type_prefix"`
}

// SiemDestinationView is the operator-visible projection. No
// plaintext secret, HasSecret instead so the UI can show "secret
// configured" without ever serializing it.
type SiemDestinationView struct {
	ID              string     `json:"id"`
	Name            string     `json:"name"`
	Type            string     `json:"type"`
	URL             string     `json:"url"`
	HasSecret       bool       `json:"has_secret"`
	Enabled         bool       `json:"enabled"`
	EventTypePrefix []string   `json:"event_type_prefix"`
	LastEventSentAt *time.Time `json:"last_event_sent_at,omitempty"`
	LastError       string     `json:"last_error,omitempty"`
	LastErrorAt     *time.Time `json:"last_error_at,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

// SiemServiceImpl is the canonical implementation.
type SiemServiceImpl struct {
	appStore applicationstore.ApplicationStore
	crypter  *siem.Crypter
	audit    AuditService // optional; nil-safe
	logger   *zap.Logger
}

// NewSiemService constructs the service. crypter MUST be non-nil
// for any write paths to work — callers without a configured
// SQUADRON_SIEM_KEY env var should pass nil here and the service
// rejects writes; reads still work so existing destinations remain
// inspectable.
func NewSiemService(
	appStore applicationstore.ApplicationStore,
	crypter *siem.Crypter,
	audit AuditService,
	logger *zap.Logger,
) SiemService {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &SiemServiceImpl{appStore: appStore, crypter: crypter, audit: audit, logger: logger}
}

func (s *SiemServiceImpl) Create(ctx context.Context, input SiemDestinationInput) (*SiemDestinationView, error) {
	if err := validateDestinationInput(input); err != nil {
		return nil, err
	}
	if s.crypter == nil {
		return nil, fmt.Errorf("SIEM crypter not configured (set %s)", siem.KeyEnvVar)
	}
	cipher, err := s.crypter.Encrypt([]byte(input.PlaintextSecret))
	if err != nil {
		return nil, fmt.Errorf("encrypt secret: %w", err)
	}
	prefixesJSON, err := json.Marshal(input.EventTypePrefix)
	if err != nil {
		return nil, fmt.Errorf("marshal prefixes: %w", err)
	}
	now := time.Now().UTC()
	d := &applicationstore.SiemDestination{
		ID:                    uuid.New().String(),
		Name:                  strings.TrimSpace(input.Name),
		Type:                  input.Type,
		URL:                   strings.TrimSpace(input.URL),
		Secret:                cipher,
		Enabled:               input.Enabled,
		EventTypePrefixesJSON: string(prefixesJSON),
		CreatedAt:             now,
		UpdatedAt:             now,
	}
	if err := s.appStore.CreateSiemDestination(ctx, d); err != nil {
		return nil, fmt.Errorf("persist destination: %w", err)
	}
	s.recordAudit(ctx, "siem.destination_created", d.ID, d.Name, map[string]any{"type": d.Type, "url": d.URL})
	return s.toView(d), nil
}

func (s *SiemServiceImpl) Get(ctx context.Context, id string) (*SiemDestinationView, error) {
	d, err := s.appStore.GetSiemDestination(ctx, id)
	if err != nil {
		return nil, err
	}
	if d == nil {
		return nil, nil
	}
	return s.toView(d), nil
}

func (s *SiemServiceImpl) List(ctx context.Context) ([]*SiemDestinationView, error) {
	stored, err := s.appStore.ListSiemDestinations(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*SiemDestinationView, len(stored))
	for i, d := range stored {
		out[i] = s.toView(d)
	}
	return out, nil
}

func (s *SiemServiceImpl) Update(ctx context.Context, id string, input SiemDestinationUpdate) (*SiemDestinationView, error) {
	existing, err := s.appStore.GetSiemDestination(ctx, id)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, fmt.Errorf("siem destination not found: %s", id)
	}
	if input.Name != nil {
		existing.Name = strings.TrimSpace(*input.Name)
	}
	if input.Type != nil {
		existing.Type = *input.Type
	}
	if input.URL != nil {
		existing.URL = strings.TrimSpace(*input.URL)
	}
	if input.Enabled != nil {
		existing.Enabled = *input.Enabled
	}
	if input.EventTypePrefix != nil {
		raw, err := json.Marshal(*input.EventTypePrefix)
		if err != nil {
			return nil, fmt.Errorf("marshal prefixes: %w", err)
		}
		existing.EventTypePrefixesJSON = string(raw)
	}
	if input.PlaintextSecret != nil {
		if *input.PlaintextSecret == "" {
			return nil, fmt.Errorf("plaintext_secret cannot be empty; omit to keep existing secret")
		}
		if s.crypter == nil {
			return nil, fmt.Errorf("SIEM crypter not configured (set %s)", siem.KeyEnvVar)
		}
		cipher, err := s.crypter.Encrypt([]byte(*input.PlaintextSecret))
		if err != nil {
			return nil, fmt.Errorf("encrypt secret: %w", err)
		}
		existing.Secret = cipher
	}
	if err := s.appStore.UpdateSiemDestination(ctx, existing); err != nil {
		return nil, fmt.Errorf("persist destination: %w", err)
	}
	s.recordAudit(ctx, "siem.destination_updated", existing.ID, existing.Name, map[string]any{"type": existing.Type, "url": existing.URL})
	return s.toView(existing), nil
}

func (s *SiemServiceImpl) Delete(ctx context.Context, id string) error {
	existing, err := s.appStore.GetSiemDestination(ctx, id)
	if err != nil {
		return err
	}
	if existing == nil {
		return fmt.Errorf("siem destination not found: %s", id)
	}
	if err := s.appStore.DeleteSiemDestination(ctx, id); err != nil {
		return err
	}
	s.recordAudit(ctx, "siem.destination_deleted", existing.ID, existing.Name, nil)
	return nil
}

// Test sends a synthetic "squadron.test" event to the destination
// and surfaces the result. Uses the same exporter the dispatcher
// would so a passing test means the next real event will land.
func (s *SiemServiceImpl) Test(ctx context.Context, id, actor string) error {
	existing, err := s.appStore.GetSiemDestination(ctx, id)
	if err != nil {
		return err
	}
	if existing == nil {
		return fmt.Errorf("siem destination not found: %s", id)
	}
	if s.crypter == nil {
		return fmt.Errorf("SIEM crypter not configured (set %s)", siem.KeyEnvVar)
	}
	secret, err := s.crypter.Decrypt(existing.Secret)
	if err != nil {
		return fmt.Errorf("decrypt secret: %w", err)
	}
	dest := s.toSiemDestination(existing)
	exporter, err := siem.BuildExporter(dest, secret)
	if err != nil {
		return fmt.Errorf("build exporter: %w", err)
	}
	now := time.Now().UTC()
	ev := siem.Event{
		ID:        uuid.New().String(),
		Timestamp: now,
		Actor:     actor,
		EventType: "squadron.test",
		Action:    "test",
		Payload:   map[string]any{"destination_id": id, "destination_name": existing.Name},
		Source:    "squadron",
	}
	if err := exporter.Send(ctx, ev); err != nil {
		// Record the failure into the destination's status so
		// the UI can show it even if the operator missed the
		// response in flight.
		errMsg := err.Error()
		_ = s.appStore.UpdateSiemDestinationStatus(ctx, id, nil, errMsg, &now)
		return err
	}
	// Mark success too so the "last sent" column updates.
	_ = s.appStore.UpdateSiemDestinationStatus(ctx, id, &now, "", nil)
	s.recordAudit(ctx, "siem.destination_tested", id, existing.Name, map[string]any{"result": "ok"})
	return nil
}

// LoadEnabled satisfies siem.SourceProvider. Returns the currently-
// enabled destinations and their decrypted secrets, parallel slices
// in matching order.
func (s *SiemServiceImpl) LoadEnabled(ctx context.Context) ([]*siem.Destination, [][]byte, error) {
	stored, err := s.appStore.ListSiemDestinations(ctx)
	if err != nil {
		return nil, nil, err
	}
	if s.crypter == nil {
		// Without a crypter we can't unwrap secrets — return empty
		// rather than failing the whole reload. The dispatcher
		// keeps existing workers if any.
		return nil, nil, nil
	}
	dests := make([]*siem.Destination, 0, len(stored))
	secrets := make([][]byte, 0, len(stored))
	for _, d := range stored {
		if !d.Enabled {
			continue
		}
		secret, err := s.crypter.Decrypt(d.Secret)
		if err != nil {
			s.logger.Warn("siem: skipping destination, decrypt failed",
				zap.String("destination", d.Name), zap.Error(err))
			continue
		}
		dests = append(dests, s.toSiemDestination(d))
		secrets = append(secrets, secret)
	}
	return dests, secrets, nil
}

// --- helpers ----------------------------------------------------------

func (s *SiemServiceImpl) toView(d *applicationstore.SiemDestination) *SiemDestinationView {
	var prefixes []string
	if d.EventTypePrefixesJSON != "" && d.EventTypePrefixesJSON != "[]" {
		_ = json.Unmarshal([]byte(d.EventTypePrefixesJSON), &prefixes)
	}
	return &SiemDestinationView{
		ID:              d.ID,
		Name:            d.Name,
		Type:            d.Type,
		URL:             d.URL,
		HasSecret:       len(d.Secret) > 0,
		Enabled:         d.Enabled,
		EventTypePrefix: prefixes,
		LastEventSentAt: d.LastEventSentAt,
		LastError:       d.LastError,
		LastErrorAt:     d.LastErrorAt,
		CreatedAt:       d.CreatedAt,
		UpdatedAt:       d.UpdatedAt,
	}
}

func (s *SiemServiceImpl) toSiemDestination(d *applicationstore.SiemDestination) *siem.Destination {
	var prefixes []string
	if d.EventTypePrefixesJSON != "" && d.EventTypePrefixesJSON != "[]" {
		_ = json.Unmarshal([]byte(d.EventTypePrefixesJSON), &prefixes)
	}
	return &siem.Destination{
		ID:              d.ID,
		Name:            d.Name,
		Type:            siem.DestinationType(d.Type),
		URL:             d.URL,
		Enabled:         d.Enabled,
		EventTypePrefix: prefixes,
		HasSecret:       len(d.Secret) > 0,
		LastEventSentAt: d.LastEventSentAt,
		LastError:       d.LastError,
		LastErrorAt:     d.LastErrorAt,
		CreatedAt:       d.CreatedAt,
		UpdatedAt:       d.UpdatedAt,
	}
}

func (s *SiemServiceImpl) recordAudit(ctx context.Context, eventType, id, name string, payload map[string]any) {
	if s.audit == nil {
		return
	}
	if payload == nil {
		payload = map[string]any{}
	}
	payload["destination_name"] = name
	_ = s.audit.Record(ctx, AuditEntry{
		Actor:      ActorSystemFromContext(ctx),
		EventType:  eventType,
		TargetType: "siem_destination",
		TargetID:   id,
		Action:     strings.TrimPrefix(eventType, "siem.destination_"),
		Payload:    payload,
	})
}

// ActorSystemFromContext extracts the auth actor, falling back to
// "system" — used by SiemService so audit events for SIEM CRUD have
// a consistent actor even when called outside an HTTP request.
func ActorSystemFromContext(ctx context.Context) string {
	if a := ActorFromContext(ctx); !a.IsZero() {
		return a.String()
	}
	return "system"
}

func validateDestinationInput(input SiemDestinationInput) error {
	if strings.TrimSpace(input.Name) == "" {
		return fmt.Errorf("name is required")
	}
	if strings.TrimSpace(input.URL) == "" {
		return fmt.Errorf("url is required")
	}
	switch input.Type {
	case string(siem.SplunkHEC), string(siem.GenericWebhook):
		// ok
	default:
		return fmt.Errorf("type must be %q or %q (got %q)", siem.SplunkHEC, siem.GenericWebhook, input.Type)
	}
	if input.PlaintextSecret == "" {
		return fmt.Errorf("plaintext_secret is required")
	}
	return nil
}
