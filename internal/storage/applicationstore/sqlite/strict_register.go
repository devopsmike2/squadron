// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package sqlite

import "github.com/devopsmike2/squadron/internal/storage/applicationstore/strict"

// init registers the SQLite backend's strict-tenant-scoping toggler in the ADR
// 0043 registry so the enterprise wire's strict.EnableAll() arms it without
// naming the backend explicitly. OSS never calls EnableAll(), so this is inert.
func init() { strict.Register("sqlite", SetStrictTenantScoping) }
