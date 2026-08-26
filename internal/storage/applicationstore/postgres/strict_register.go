// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package postgres

import "github.com/devopsmike2/squadron/internal/storage/applicationstore/strict"

// init registers the Postgres backend's strict-tenant-scoping toggler in the ADR
// 0043 registry. This is the fix at the heart of ADR 0043: before it, the
// enterprise wire armed strict scoping on SQLite only and the Postgres seam —
// the backend enterprise HA actually runs on — stayed dead. Registered here, the
// enterprise wire's strict.EnableAll() arms Postgres too. OSS never calls
// EnableAll(), so this is inert.
func init() { strict.Register("postgres", SetStrictTenantScoping) }
