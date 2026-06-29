// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package github

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestListTree_recursive_returns_blobs(t *testing.T) {
	cli, done := newFakeGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/repos/octo/widgets/git/trees/") {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.URL.Query().Get("recursive") != "1" {
			t.Errorf("recursive query missing: %q", r.URL.RawQuery)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"sha":"abc","truncated":false,"tree":[
			{"path":"main.tf","type":"blob"},
			{"path":"modules","type":"tree"},
			{"path":"modules/storage.tf","type":"blob"}
		]}`))
	})
	defer done()

	entries, err := cli.ListTree(context.Background(), "octo", "widgets", "main")
	if err != nil {
		t.Fatalf("ListTree error: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3", len(entries))
	}
	var blobs []string
	for _, e := range entries {
		if e.Type == "blob" {
			blobs = append(blobs, e.Path)
		}
	}
	if len(blobs) != 2 || blobs[0] != "main.tf" || blobs[1] != "modules/storage.tf" {
		t.Errorf("blobs = %v, want [main.tf modules/storage.tf]", blobs)
	}
}

func TestListTree_404_maps_to_ErrRepoNotFound(t *testing.T) {
	cli, done := newFakeGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	})
	defer done()

	_, err := cli.ListTree(context.Background(), "octo", "widgets", "nope")
	if err != ErrRepoNotFound {
		t.Errorf("err = %v, want ErrRepoNotFound", err)
	}
}
