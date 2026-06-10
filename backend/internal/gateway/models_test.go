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

func TestLookupModelSpecUnknownModelFallsBackToOpus(t *testing.T) {
	modelID, spec := LookupModelSpec("totally-unknown-model")

	if modelID != "claude-opus-4-8" {
		t.Fatalf("modelID = %q, want claude-opus-4-8", modelID)
	}
	if spec.Name != "Claude Opus 4.8" {
		t.Fatalf("spec.Name = %q, want Claude Opus 4.8", spec.Name)
	}
}

func TestDefaultModelListStartsWithLatestModel(t *testing.T) {
	if len(defaultModelList) == 0 {
		t.Fatal("defaultModelList is empty")
	}
	if defaultModelList[0].ID != "claude-fable-5" {
		t.Fatalf("first model = %q, want claude-fable-5", defaultModelList[0].ID)
	}
}

func TestLookupModelSpecFableKeywordFallback(t *testing.T) {
	modelID, spec := LookupModelSpec("some-unknown-fable-model")

	if modelID != "claude-fable-5" {
		t.Fatalf("modelID = %q, want claude-fable-5", modelID)
	}
	if spec.Name != "Claude Fable 5" {
		t.Fatalf("spec.Name = %q, want Claude Fable 5", spec.Name)
	}
}
