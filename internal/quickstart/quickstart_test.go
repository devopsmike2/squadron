// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package quickstart

import (
	"strings"
	"testing"
)

// TestCatalog — the registry must be non-empty and every entry
// must be renderable as a starter config. Catches accidental
// catalog/template drift in code review.
func TestCatalog(t *testing.T) {
	cat := Catalog()
	if len(cat) == 0 {
		t.Fatal("Catalog must not be empty")
	}
	for _, info := range cat {
		if info.Name == "" {
			t.Errorf("backend %q has empty Name", info.ID)
		}
		if info.Description == "" {
			t.Errorf("backend %q has empty Description", info.ID)
		}
		// Every catalog entry must produce a renderable template —
		// otherwise the UI's backend picker offers an option that
		// 500s on click.
		yaml, err := StarterConfig(info.ID, "ws://test/v1/opamp")
		if err != nil {
			t.Errorf("backend %q has no starter template: %v", info.ID, err)
			continue
		}
		if !strings.Contains(yaml, "extensions:") {
			t.Errorf("backend %q starter is missing extensions block", info.ID)
		}
		if !strings.Contains(yaml, "service:") {
			t.Errorf("backend %q starter is missing service block", info.ID)
		}
		if !strings.Contains(yaml, "opamp") {
			t.Errorf("backend %q starter doesn't enable the opamp extension", info.ID)
		}
		// The URL substitution must have happened — no template
		// placeholders should leak through.
		if strings.Contains(yaml, "{{OPAMP_SERVER_URL}}") {
			t.Errorf("backend %q starter still has the {{OPAMP_SERVER_URL}} placeholder", info.ID)
		}
		// Env vars referenced in the YAML should appear in the
		// catalog's EnvVars hint (catches forgotten env-var docs).
		// Skip for backends that have no required env vars.
		for _, ev := range info.EnvVars {
			if ev.Required && !strings.Contains(yaml, ev.Name) {
				t.Errorf("backend %q claims to require %s but the YAML doesn't reference it",
					info.ID, ev.Name)
			}
		}
	}
}

// TestStarterConfig_URLSubstitution — the OpAMP URL is wired into
// the rendered YAML.
func TestStarterConfig_URLSubstitution(t *testing.T) {
	url := "ws://squadron.example.com:4320/v1/opamp"
	yaml, err := StarterConfig(BackendHoneycomb, url)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(yaml, url) {
		t.Errorf("rendered YAML doesn't contain the OpAMP URL: %s", url)
	}
}

// TestStarterConfig_UnknownBackend.
func TestStarterConfig_UnknownBackend(t *testing.T) {
	_, err := StarterConfig(Backend("nope"), "ws://x/v1/opamp")
	if err == nil {
		t.Errorf("expected error for unknown backend")
	}
}

// TestStarterConfig_RequiresURL.
func TestStarterConfig_RequiresURL(t *testing.T) {
	_, err := StarterConfig(BackendDatadog, "")
	if err == nil {
		t.Errorf("expected error for empty URL")
	}
}

// TestOpAMPExtensionSnippet — the adoption snippet must contain
// the URL, an extensions block, AND a service.extensions
// reference (otherwise the extension is defined but not enabled).
func TestOpAMPExtensionSnippet(t *testing.T) {
	url := "ws://squadron:4320/v1/opamp"
	out, err := OpAMPExtensionSnippet(url)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, url) {
		t.Errorf("snippet missing URL")
	}
	if !strings.Contains(out, "extensions:") {
		t.Errorf("snippet missing extensions block")
	}
	if !strings.Contains(out, "opamp:") {
		t.Errorf("snippet missing opamp definition")
	}
	if !strings.Contains(out, "service:") {
		t.Errorf("snippet missing service section")
	}
	if !strings.Contains(out, "extensions: [opamp]") {
		t.Errorf("snippet must enable opamp in service.extensions, otherwise the collector ignores it")
	}
	if !strings.Contains(out, "#") {
		t.Errorf("snippet should include header comments explaining the merge")
	}
}

func TestOpAMPExtensionSnippet_RequiresURL(t *testing.T) {
	_, err := OpAMPExtensionSnippet("")
	if err == nil {
		t.Errorf("expected error for empty URL")
	}
}

// TestAllBackendsHaveCatalogEntry confirms the AllBackends list
// stays in sync with backendInfoFor.
func TestAllBackendsHaveCatalogEntry(t *testing.T) {
	for _, b := range AllBackends {
		info := backendInfoFor(b)
		if info.Name == "" || info.Name == string(b) {
			t.Errorf("backend %q missing catalog metadata", b)
		}
	}
}
