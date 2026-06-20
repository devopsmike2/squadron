// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package aws implements the slice-1 AWS scanner for Squadron's
// universal-observation discovery arc. Scope is strictly read-only:
// the role's trust policy must include sts:AssumeRole only, and the
// permissions policy is limited to ec2:Describe*, lambda:List*, and
// lambda:Get* per docs/universal-discovery-design.md.
//
// Sessions are in-memory only. Per the security architecture, the
// short-lived STS credentials this package issues are never written
// to disk and are dropped after each scan completes.
//
// This is the ONLY package in the slice-1 codebase that imports the
// AWS SDK. If a future feature needs AWS, it lives here (for
// discovery) or in a brand-new internal/remediation/aws (for the
// post-slice-6 remediation posture, which has a different threat
// model and is not allowed to share imports with this package).
package aws
