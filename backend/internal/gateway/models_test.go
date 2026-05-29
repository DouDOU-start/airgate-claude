package gateway

import "testing"

func TestLookupModelSpecUsesLatestOpusForFamilyFallback(t *testing.T) {
	modelID, spec := LookupModelSpec("some-unknown-opus-model")

	if modelID != "claude-opus-4-8" {
		t.Fatalf("modelID = %q, want claude-opus-4-8", modelID)
	}
	if spec.Name != "Claude Opus 4.8" {
		t.Fatalf("spec.Name = %q, want Claude Opus 4.8", spec.Name)
	}
}

func TestDefaultModelListStartsWithLatestOpus(t *testing.T) {
	if len(defaultModelList) == 0 {
		t.Fatal("defaultModelList is empty")
	}
	if defaultModelList[0].ID != "claude-opus-4-8" {
		t.Fatalf("first model = %q, want claude-opus-4-8", defaultModelList[0].ID)
	}
}
