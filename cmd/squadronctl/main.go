// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// squadronctl is the Squadron command-line client. It wraps the REST
// API at /api/v1 so operators can script Squadron from CI pipelines,
// shell scripts, and ad-hoc terminals without opening the UI.
//
// Configuration sources, in order of precedence (highest wins):
//
//   1. Command-line flags: --server, --token
//   2. Environment variables: SQUADRON_URL, SQUADRON_TOKEN
//   3. ~/.squadronctl/config.yaml (optional)
//
// All commands accept -o json for machine-readable output. Without it
// they print human-friendly tables and prose. Non-zero exit codes
// indicate failure; specific codes signal specific failure modes —
// see each command's docs.
package main

import (
	"fmt"
	"os"

	"github.com/devopsmike2/squadron/cmd/squadronctl/commands"
)

func main() {
	root := commands.NewRootCommand()
	if err := root.Execute(); err != nil {
		// cobra already prints the error; we just need the right exit.
		fmt.Fprintln(os.Stderr, "")
		os.Exit(1)
	}
}
