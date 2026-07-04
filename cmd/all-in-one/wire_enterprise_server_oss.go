// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build !enterprise

// wire_enterprise_server_oss.go is the default open-core wiring for the
// enterprise RBAC management handler seam (ADR 0010 slice 2b). Built when the
// `enterprise` build tag is NOT set.
//
// It is a no-op: OSS never installs an EnterpriseRBACHandler, so the
// late-bound /api/v1/rbac/* routes return 404 ("RBAC management is an
// enterprise feature"). The seam exists only so the enterprise edition can
// call server.SetEnterpriseRBACHandler against the same single main.go call
// site without any change to OSS behavior.
//
// The enterprise edition ships a parallel wire_enterprise_server_enterprise.go
// (build tag: enterprise) that installs the real handler. Both files expose the
// same enterpriseServerWiring symbol so main.go has a single call site.
// Mirrors the no-op posture of identityProviders() / scopedApplicationStore().

package main

import "github.com/devopsmike2/squadron/internal/api"

// enterpriseServerWiring installs the edition's post-construction API server
// wiring. The OSS build does nothing (RBAC management routes stay 404).
func enterpriseServerWiring(server *api.Server) {}
