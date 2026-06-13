// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package deploy

import (
	"bufio"
	"bytes"
	"strings"
)

// ParseInventoryHosts extracts hostnames (and IP literals) from a
// standard Ansible INI-format inventory file.
//
// The accepted shape:
//
//	[windows]
//	host01
//	host02.example.com
//	10.10.40.7        ;  (also OK)
//	# this is a comment
//	GAXGPAP158UA ansible_user=foo      ; inline vars — first token is the host
//
//	[windows:vars]
//	ansible_user=...                  ; var-only line, skipped
//
// Rules:
//   - Blank lines are skipped.
//   - Lines starting with `#` or `;` are skipped (comments).
//   - Section headers `[group]`, `[group:vars]`, `[group:children]`
//     are detected; we DON'T return hosts from `:vars` or `:children`
//     sections because those don't list hosts.
//   - A line where the first token contains `=` is a var-only line
//     and is skipped.
//   - Otherwise the first whitespace-delimited token is taken as the
//     hostname (inline vars after the host are ignored).
//   - Output is unique (case-insensitive dedup) and preserves the
//     order each host first appeared in the file.
//
// Caller is responsible for whatever case + FQDN normalization
// they care about; v0.32 inventory reconciliation does its own
// lowercase + short-name comparison.
func ParseInventoryHosts(content []byte) []string {
	var (
		out         []string
		seen        = map[string]struct{}{}
		currentSect = ""
		skipSection = false
	)
	sc := bufio.NewScanner(bytes.NewReader(content))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		// Section header
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			currentSect = strings.TrimSpace(line[1 : len(line)-1])
			// "windows:vars" / "windows:children" don't list hosts.
			skipSection = strings.Contains(currentSect, ":")
			continue
		}
		if skipSection {
			continue
		}
		// Strip inline comments after a `#` or `;` so they don't
		// stick to the hostname when there's no space before them.
		if idx := strings.IndexAny(line, "#;"); idx >= 0 {
			line = strings.TrimSpace(line[:idx])
			if line == "" {
				continue
			}
		}
		parts := strings.Fields(line)
		if len(parts) == 0 {
			continue
		}
		// Var-only line: first token contains '='
		if strings.Contains(parts[0], "=") {
			continue
		}
		host := parts[0]
		key := strings.ToLower(host)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, host)
	}
	return out
}
