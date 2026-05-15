// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package commands

import (
	"fmt"

	"github.com/spf13/cobra"
)

// Version is overridden at link time by the release workflow:
//
//   go build -ldflags "-X .../commands.Version=v0.9.0"
//
// Defaults to "dev" so unreleased builds make it clear.
var Version = "dev"

func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print squadronctl version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Println(Version)
		},
	}
}
