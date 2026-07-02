package agentid

import (
	"testing"

	"github.com/google/uuid"
)

// TestDerive pins the widened agent-identity resolution: agents that don't emit
// a UUID service.instance.id still get a deterministic UUID so passive OTLP
// discovery, telemetry keying, and OpAMP registration can all agree on one id.
func TestDerive(t *testing.T) {
	// 1. UUID service.instance.id is used verbatim.
	u := "756d93e3-9e49-44fa-b0b4-0e2da2f3ca2a"
	if got := Derive(map[string]string{"service.instance.id": u}); got != u {
		t.Fatalf("uuid service.instance.id: got %q want %q", got, u)
	}

	// 2. Non-UUID service.instance.id -> stable, valid UUID.
	a := Derive(map[string]string{"service.instance.id": "collector-7"})
	b := Derive(map[string]string{"service.instance.id": "collector-7"})
	if a != b {
		t.Fatalf("non-uuid instance.id not deterministic: %q vs %q", a, b)
	}
	if _, err := uuid.Parse(a); err != nil {
		t.Fatalf("non-uuid instance.id did not yield a UUID: %q (%v)", a, err)
	}

	// 3. host.name -> stable UUID; distinct hosts get distinct ids.
	h1 := Derive(map[string]string{"host.name": "ip-10-0-0-1"})
	h1b := Derive(map[string]string{"host.name": "ip-10-0-0-1"})
	h2 := Derive(map[string]string{"host.name": "ip-10-0-0-2"})
	if h1 != h1b {
		t.Fatalf("host.name not deterministic: %q vs %q", h1, h1b)
	}
	if h1 == h2 {
		t.Fatalf("distinct hosts collided to same agent id: %q", h1)
	}
	if _, err := uuid.Parse(h1); err != nil {
		t.Fatalf("host.name did not yield a UUID: %q (%v)", h1, err)
	}

	// 4. service.name fallback.
	s := Derive(map[string]string{"service.name": "checkout"})
	if _, err := uuid.Parse(s); err != nil {
		t.Fatalf("service.name did not yield a UUID: %q (%v)", s, err)
	}

	// 5. Precedence: a UUID service.instance.id wins over host.name.
	withBoth := Derive(map[string]string{"service.instance.id": u, "host.name": "ip-10-0-0-1"})
	if withBoth != u {
		t.Fatalf("instance.id should win over host.name: got %q", withBoth)
	}

	// 6. Truly anonymous input keeps the legacy sentinel.
	if got := Derive(map[string]string{}); got != "default" {
		t.Fatalf("empty attrs: got %q want \"default\"", got)
	}

	// 7. Frozen namespace guard: a known seed must hash to a known UUID. If this
	//    breaks, the namespace changed and every derived agent id would shift.
	if got := Derive(map[string]string{"host.name": "freeze-check"}); got != stableUUID("host.name:freeze-check") {
		t.Fatalf("namespace drift detected: %q", got)
	}
}
