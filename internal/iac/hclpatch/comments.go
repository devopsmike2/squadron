// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package hclpatch

import (
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
)

// StripComments removes every comment from a Terraform/HCL source
// snippet while preserving the code and its structure, returning the
// reformatted result.
//
// It tokenizes via hclwrite and drops comment tokens, which makes it
// safe against the classic regex trap: a `#` or `//` inside a string
// literal (e.g. a URL or a user_data heredoc) is a string token, not
// a comment token, so it is never touched.
//
// Comment-token handling:
//   - A line comment ("# ..." or "// ...") carries its trailing
//     newline in the token bytes. Dropping it outright would fuse the
//     next line onto the previous one, so we replace it with a bare
//     newline token to keep line structure intact.
//   - A block comment ("/* ... */") carries no trailing newline; it is
//     dropped and the surrounding formatting is normalised by
//     hclwrite.Format.
//
// Failure mode is intentionally non-destructive: if the snippet does
// not parse as HCL (a malformed proposer emission), StripComments
// returns the input unchanged rather than risk corrupting it. The
// caller is choosing cleaner output, not correctness — so "leave it
// exactly as it was" is the safe default.
func StripComments(src []byte) []byte {
	f, diags := hclwrite.ParseConfig(src, "snippet.tf", hcl.Pos{Line: 1, Column: 1})
	if diags.HasErrors() {
		return src
	}
	tokens := f.BuildTokens(nil)
	filtered := make(hclwrite.Tokens, 0, len(tokens))
	for _, t := range tokens {
		if t.Type == hclsyntax.TokenComment {
			if n := len(t.Bytes); n > 0 && t.Bytes[n-1] == '\n' {
				// Preserve the line break a line comment was sitting on.
				filtered = append(filtered, &hclwrite.Token{
					Type:  hclsyntax.TokenNewline,
					Bytes: []byte("\n"),
				})
			}
			continue
		}
		filtered = append(filtered, t)
	}
	return hclwrite.Format(filtered.Bytes())
}
