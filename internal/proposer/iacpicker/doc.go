// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package iacpicker picks which Terraform pattern the
// trace-emission-* recommendation should extend in the
// operator's IaC repo. The picker reads the existing repo
// contents (provided as a string by the caller) and decides
// whether to extend an existing block or introduce a new one.
//
// Falls back to a documented default pattern when the repo
// content can't be parsed or doesn't contain a related block.
// See docs/proposals/trace-integration-slice2.md §5.
package iacpicker
