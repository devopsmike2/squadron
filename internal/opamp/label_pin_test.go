// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package opamp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/services"
)

// fakeLabelPinResolver returns fixed env/cluster pins for any token (ADR 0056
// test double).
type fakeLabelPinResolver struct{ env, cluster string }

func (f fakeLabelPinResolver) PinnedLabels(_ *services.APIToken) (string, string) {
	return f.env, f.cluster
}

// TestAuthenticateConn_LabelPinPopulatedWhenResolverSet: with a label-pin resolver
// installed (enterprise), an authenticated connection carries the resolved
// env/cluster.
func TestAuthenticateConn_LabelPinPopulatedWhenResolverSet(t *testing.T) {
	s := &Server{
		logger:           zap.NewNop(),
		auth:             &fakeAuthService{token: enrollToken("acme")},
		labelPinResolver: fakeLabelPinResolver{env: "prod", cluster: "us-west-2"},
	}
	auth, accept := s.authenticateConn(context.Background(), bearerReq(t, "Bearer sqd_valid", ""))
	assert.True(t, accept)
	assert.True(t, auth.authenticated)
	assert.Equal(t, "prod", auth.pinnedEnv, "env pin resolved from the token when a resolver is set")
	assert.Equal(t, "us-west-2", auth.pinnedCluster, "cluster pin resolved from the token when a resolver is set")
}

// TestAuthenticateConn_NoLabelPinWhenNoResolver: the OSS default (no resolver)
// leaves the connection unpinned — label pinning is inert in OSS.
func TestAuthenticateConn_NoLabelPinWhenNoResolver(t *testing.T) {
	s := &Server{logger: zap.NewNop(), auth: &fakeAuthService{token: enrollToken("acme")}}
	auth, _ := s.authenticateConn(context.Background(), bearerReq(t, "Bearer sqd_valid", ""))
	assert.Equal(t, "", auth.pinnedEnv, "no resolver ⇒ env unpinned (OSS inert)")
	assert.Equal(t, "", auth.pinnedCluster, "no resolver ⇒ cluster unpinned (OSS inert)")
}

// TestResolveLabels covers the ADR 0056 relabel-and-log semantics: unpinned is a
// no-op, a pinned field overwrites the reported value, a matching value is a
// no-op, and each field pins independently.
func TestResolveLabels(t *testing.T) {
	s := &Server{logger: zap.NewNop()}

	// Unpinned context ⇒ labels unchanged (OSS path).
	in := map[string]string{labelKeyEnv: "staging", labelKeyCluster: "eu-1"}
	got := s.resolveLabels(context.Background(), in)
	assert.Equal(t, "staging", got[labelKeyEnv])
	assert.Equal(t, "eu-1", got[labelKeyCluster])

	// Pin differs ⇒ PIN wins (relabel-and-log), for both fields.
	ctx := withPinnedLabels(context.Background(), "prod", "us-west-2")
	got = s.resolveLabels(ctx, map[string]string{labelKeyEnv: "staging", labelKeyCluster: "eu-1"})
	assert.Equal(t, "prod", got[labelKeyEnv], "env pin overrides the reported label")
	assert.Equal(t, "us-west-2", got[labelKeyCluster], "cluster pin overrides the reported label")

	// Pinned value stamps even when the agent reported nothing for that key.
	got = s.resolveLabels(withPinnedLabels(context.Background(), "prod", ""), map[string]string{})
	assert.Equal(t, "prod", got[labelKeyEnv], "env pin stamps an absent label")
	_, hasCluster := got[labelKeyCluster]
	assert.False(t, hasCluster, "an unpinned field is not stamped")

	// Partial pin: only cluster pinned ⇒ env untouched (ADR 0056 Q4).
	got = s.resolveLabels(withPinnedLabels(context.Background(), "", "us-west-2"),
		map[string]string{labelKeyEnv: "staging"})
	assert.Equal(t, "staging", got[labelKeyEnv], "env left as reported when only cluster is pinned")
	assert.Equal(t, "us-west-2", got[labelKeyCluster])

	// Matching pin ⇒ value unchanged (no mismatch).
	got = s.resolveLabels(withPinnedLabels(context.Background(), "prod", ""),
		map[string]string{labelKeyEnv: "prod"})
	assert.Equal(t, "prod", got[labelKeyEnv])

	// Nil label map with a pin ⇒ map is created and stamped (no panic).
	got = s.resolveLabels(withPinnedLabels(context.Background(), "prod", ""), nil)
	assert.Equal(t, "prod", got[labelKeyEnv])
}
