package services

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestGroupMarshalJSON_NilLabelsSerializesToEmptyObject pins the
// defense-in-depth serializer fix for the Southern-pilot blank-page
// bug: GET /api/v1/groups returned `labels: null` for a group that
// never had labels, and the UI's unguarded Object.entries(null) crashed
// the whole route. The API must emit `"labels":{}` for such a group so
// the wire shape is internally consistent (other groups already
// serialize `{}`) and no client can be handed a null map.
func TestGroupMarshalJSON_NilLabelsSerializesToEmptyObject(t *testing.T) {
	g := Group{ID: "southern-pilot", Name: "Southern Pilot"} // Labels left nil

	raw, err := json.Marshal(g)
	if err != nil {
		t.Fatalf("marshal group: %v", err)
	}
	got := string(raw)

	if !strings.Contains(got, `"labels":{}`) {
		t.Fatalf("nil labels must serialize to an empty object; got %s", got)
	}
	if strings.Contains(got, `"labels":null`) {
		t.Fatalf("labels must never serialize to null; got %s", got)
	}
}

// TestGroupMarshalJSON_PopulatedLabelsPreserved confirms the
// normalization does not disturb a group that DOES carry labels.
func TestGroupMarshalJSON_PopulatedLabelsPreserved(t *testing.T) {
	g := Group{
		ID:     "web-prod",
		Name:   "Web Prod",
		Labels: map[string]string{"env": "prod", "team": "sre"},
	}

	raw, err := json.Marshal(g)
	if err != nil {
		t.Fatalf("marshal group: %v", err)
	}
	got := string(raw)

	if !strings.Contains(got, `"env":"prod"`) || !strings.Contains(got, `"team":"sre"`) {
		t.Fatalf("populated labels must round-trip; got %s", got)
	}
}

// TestGroupMarshalJSON_EmptyMapStillEmptyObject guards the already-
// consistent path so a future refactor can't regress it.
func TestGroupMarshalJSON_EmptyMapStillEmptyObject(t *testing.T) {
	g := Group{ID: "empty", Name: "Empty", Labels: map[string]string{}}

	raw, err := json.Marshal(g)
	if err != nil {
		t.Fatalf("marshal group: %v", err)
	}
	if got := string(raw); !strings.Contains(got, `"labels":{}`) {
		t.Fatalf("empty-map labels must serialize to an empty object; got %s", got)
	}
}
