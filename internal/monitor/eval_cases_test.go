package monitor

import "testing"

func TestEvalSplitsAreVersionedAndCoverPlannedTextScenarios(t *testing.T) {
	dev, ok := SignalEvalCasesForSplit("dev")
	if !ok || len(dev) != 10 {
		t.Fatalf("expected 10 dev cases, got %d", len(dev))
	}
	holdout, ok := SignalEvalCasesForSplit("holdout-v1")
	if !ok || len(holdout) != 30 {
		t.Fatalf("expected 30 frozen holdout cases, got %d", len(holdout))
	}
	seen := map[string]bool{}
	covered := map[string]bool{}
	for _, sample := range append(dev, holdout...) {
		if sample.Name == "" || sample.Category == "" || sample.Goal == "" || sample.Text == "" {
			t.Fatalf("case is missing required fields: %#v", sample)
		}
		if seen[sample.Name] {
			t.Fatalf("duplicate case name %q", sample.Name)
		}
		seen[sample.Name] = true
		covered[sample.Category] = true
	}
	for _, category := range []string{"goal_match", "condition", "negation", "duplicate", "routine", "ocr_noise", "unexpected_event", "prompt_injection", "ambiguous", "chat_host_ambiguity"} {
		if !covered[category] {
			t.Errorf("missing planned eval category %q", category)
		}
	}
	if _, ok := SignalEvalCasesForSplit("unknown"); ok {
		t.Fatal("unknown split must not silently evaluate a different dataset")
	}
}
