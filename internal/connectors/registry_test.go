// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package connectors

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRegistryStartsEmpty asserts the OSS-inert posture: a fresh
// registry has no connector types, so nothing is selectable until a
// connector registers.
func TestRegistryStartsEmpty(t *testing.T) {
	r := NewRegistry()
	assert.Empty(t, r.Types())
	assert.False(t, r.Has(mockType))
}

// TestRegistryRegisterAndInstantiate covers the happy path: register a
// type, then instantiate a connector from a stored Config.
func TestRegistryRegisterAndInstantiate(t *testing.T) {
	r := NewRegistry()
	require.NoError(t, r.Register(mockType, newMockFactory(nil)))

	assert.True(t, r.Has(mockType))
	assert.Equal(t, []string{mockType}, r.Types())

	cfg := Config{ID: "c1", Type: mockType, DisplayName: "Test", Endpoint: "http://mock"}
	conn, err := r.New(cfg, ConnectorCredentials{Token: "t"})
	require.NoError(t, err)
	require.NotNil(t, conn)
	assert.Equal(t, mockType, conn.Describe().Type)
}

// TestRegistryUnknownType covers instantiating a type that was never
// registered.
func TestRegistryUnknownType(t *testing.T) {
	r := NewRegistry()
	_, err := r.New(Config{ID: "c1", Type: "nope", Endpoint: "http://mock"}, ConnectorCredentials{})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUnknownType)
}

// TestRegistryDuplicateRegistration asserts registration is fail-loud,
// not last-write-wins.
func TestRegistryDuplicateRegistration(t *testing.T) {
	r := NewRegistry()
	require.NoError(t, r.Register(mockType, newMockFactory(nil)))
	err := r.Register(mockType, newMockFactory(nil))
	assert.ErrorIs(t, err, ErrDuplicateType)
}

// TestRegistryRegisterValidation covers the nil-factory and empty-type
// guards.
func TestRegistryRegisterValidation(t *testing.T) {
	r := NewRegistry()
	assert.ErrorIs(t, r.Register("", newMockFactory(nil)), ErrEmptyType)
	assert.ErrorIs(t, r.Register(mockType, nil), ErrNilFactory)
}

// TestRegistryFactoryErrorPropagates asserts a Factory error (here, a
// missing endpoint) surfaces through New rather than being swallowed.
func TestRegistryFactoryErrorPropagates(t *testing.T) {
	r := NewRegistry()
	require.NoError(t, r.Register(mockType, newMockFactory(nil)))
	_, err := r.New(Config{ID: "c1", Type: mockType /* no endpoint */}, ConnectorCredentials{})
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrUnknownType)
}
