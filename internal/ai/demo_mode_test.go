package ai

import (
	"context"
	"strings"
	"testing"

	"go.uber.org/zap"
)

// keylessDemoService returns a service with no API key and demo mode on — the
// exact state the one-click demo puts it in on a keyless install.
func keylessDemoService(t *testing.T) *Service {
	t.Helper()
	s := NewService(Config{Enabled: false}, zap.NewNop())
	s.SetDemoMode(true)
	if !s.demoActive() {
		t.Fatal("expected demoActive() true for keyless + demo mode on")
	}
	return s
}

func TestDemoActive_RealKeyWins(t *testing.T) {
	// A real key must always take precedence: demo mode never engages.
	s := NewService(Config{Enabled: true, APIKey: "sk-ant-fake"}, zap.NewNop())
	s.SetDemoMode(true)
	if s.demoActive() {
		t.Fatal("demoActive() must be false when a real API key is configured")
	}
	if !s.Enabled() {
		t.Fatal("Enabled() should be true with a key")
	}
}

func TestDemoCapabilities_ReportsCapable(t *testing.T) {
	s := keylessDemoService(t)
	caps := s.Capabilities()
	if !caps.Enabled {
		t.Fatal("demo mode should report Capabilities.Enabled=true so the UI shows AI affordances")
	}
	if caps.ExplainModel != "demo" {
		t.Fatalf("expected demo model marker, got %q", caps.ExplainModel)
	}
}

func TestAskDemoMode_GroundedCitations(t *testing.T) {
	s := keylessDemoService(t)
	bag := map[string]string{
		"spike:spike-demo-123":         "critical cost spike +312% metrics",
		"rollout:rlo-demo-ai-proposal": "AI-proposed rollout pin hashing.rounds",
		"agent:agent-abc":              "web agent online",
	}
	res, err := s.Ask(context.Background(), AskInput{
		Question: "What is driving my costs and is anything being done?",
		Context:  bag,
	})
	if err != nil {
		t.Fatalf("Ask (demo): %v", err)
	}
	if res.Model != "demo-grounded" {
		t.Fatalf("expected demo-grounded model, got %q", res.Model)
	}
	if strings.TrimSpace(res.Answer) == "" {
		t.Fatal("demo answer should be non-empty")
	}
	// Must cite only ids that were in the bag (grounded, not fabricated).
	if len(res.Citations) == 0 {
		t.Fatal("expected at least one grounded citation")
	}
	for _, c := range res.Citations {
		key := c.Kind + ":" + c.ID
		if _, ok := bag[key]; !ok {
			t.Fatalf("citation %q not present in the context bag (fabricated)", key)
		}
	}
	// A cost question should surface the spike.
	sawSpike := false
	for _, c := range res.Citations {
		if c.Kind == "spike" {
			sawSpike = true
		}
	}
	if !sawSpike {
		t.Fatal("a cost question should cite the cost spike")
	}
}

func TestAskDemoMode_EmptyQuestion(t *testing.T) {
	s := keylessDemoService(t)
	if _, err := s.Ask(context.Background(), AskInput{Question: "  "}); err == nil {
		t.Fatal("expected error for empty question")
	}
}

func TestExplainSnippetDemo(t *testing.T) {
	s := keylessDemoService(t)
	res, err := s.ExplainSnippet(context.Background(), ExplainSnippetRequest{
		Snippet: "processors:\n  attributes/drop:\n    actions:\n      - key: http.url\n        action: delete",
	})
	if err != nil {
		t.Fatalf("ExplainSnippet (demo): %v", err)
	}
	if res.Model != "demo-grounded" || strings.TrimSpace(res.Explanation) == "" {
		t.Fatalf("bad demo explain response: model=%q explanation=%q", res.Model, res.Explanation)
	}
	if !strings.Contains(strings.ToLower(res.Explanation), "attribute") {
		t.Fatalf("attribute-drop snippet should mention attributes, got: %s", res.Explanation)
	}
}

func TestMergeIntoConfigDemo(t *testing.T) {
	s := keylessDemoService(t)
	res, err := s.MergeIntoConfig(context.Background(), MergeIntoConfigRequest{
		BaseYAML:    "receivers:\n  otlp:\n",
		SnippetYAML: "processors:\n  batch: {}",
	})
	if err != nil {
		t.Fatalf("MergeIntoConfig (demo): %v", err)
	}
	if res.Model != "demo-grounded" {
		t.Fatalf("expected demo-grounded, got %q", res.Model)
	}
	if !strings.Contains(res.MergedYAML, "otlp") || !strings.Contains(res.MergedYAML, "batch") {
		t.Fatalf("merged YAML should contain both base and snippet, got: %s", res.MergedYAML)
	}
}
