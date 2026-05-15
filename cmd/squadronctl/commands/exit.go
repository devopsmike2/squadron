// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package commands

import (
	"io"
	"os"
)

// exitFn and stderrW are package-level seams that tests override. In
// production they call os.Exit directly so the CLI can return
// distinguishable exit codes from `rollouts wait` etc. — cobra's
// RunE return-value path only ever yields exit 1, which isn't enough
// to differentiate "rollout failed" from "wait timed out".
var (
	exitFn  func(int) = os.Exit
	stderrW io.Writer = os.Stderr
)
