// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package commands

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

// prettyJSON pretty-prints v with two-space indent. Used by every -o
// json path so output is stable across commands.
func prettyJSON(v any) (string, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// table writes rows to w as a tab-separated table with a header row.
// Used by list commands for human-friendly output without pulling in
// a third-party table library.
//
// header is one slice; rows is a slice of slices, all of which must
// have the same length as header. Empty rows render as an empty
// table (just the header).
func table(w io.Writer, header []string, rows [][]string) {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, strings.Join(header, "\t"))
	for _, r := range rows {
		fmt.Fprintln(tw, strings.Join(r, "\t"))
	}
	_ = tw.Flush()
}

// truncate clips s to n chars with a trailing ellipsis. Used in tables
// to keep ID columns narrow without losing the prefix you actually use
// to identify the row.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}
