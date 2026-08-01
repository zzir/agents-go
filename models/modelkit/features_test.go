package modelkit

import (
	"errors"
	"strings"
	"testing"

	"github.com/zzir/agents-go/agents"
)

func TestRejectIgnoresUnusedFeatures(t *testing.T) {
	req := agents.ModelRequest{Settings: &agents.ModelSettings{Temperature: agents.Ptr(0.2)}}
	if err := Reject("prov", req, FeatureServiceTier, FeatureVerbosity, FeaturePreviousResponseID); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRejectNamesTheFeature(t *testing.T) {
	req := agents.ModelRequest{Settings: &agents.ModelSettings{ServiceTier: agents.ServiceTierFlex}}
	err := Reject("prov", req, FeatureServiceTier)
	if err == nil {
		t.Fatal("expected error")
	}
	var ue *agents.UserError
	if !errors.As(err, &ue) {
		t.Fatalf("expected *agents.UserError, got %T", err)
	}
	if !strings.Contains(err.Error(), "service_tier") || !strings.Contains(err.Error(), "prov") {
		t.Fatalf("error should name provider and feature: %v", err)
	}
}

func TestRejectRequestLevelFeatures(t *testing.T) {
	req := agents.ModelRequest{PreviousResponseID: "resp_1"}
	if err := Reject("prov", req, FeaturePreviousResponseID); err == nil {
		t.Fatal("expected error for previous_response_id")
	}
	req = agents.ModelRequest{ConversationID: "conv_1"}
	if err := Reject("prov", req, FeatureConversationID); err == nil {
		t.Fatal("expected error for conversation_id")
	}
	req = agents.ModelRequest{Prompt: &agents.Prompt{ID: "p"}}
	if err := Reject("prov", req, FeaturePrompt); err == nil {
		t.Fatal("expected error for prompt")
	}
}

func TestRejectReasoningSummaryIsSeparateFromReasoning(t *testing.T) {
	req := agents.ModelRequest{Settings: &agents.ModelSettings{
		Reasoning: &agents.Reasoning{Effort: agents.ReasoningEffortHigh},
	}}
	if err := Reject("prov", req, FeatureReasoningSummary); err != nil {
		t.Fatalf("effort alone must not trip reasoning.summary: %v", err)
	}
	req.Settings.Reasoning.Summary = agents.ReasoningSummaryAuto
	if err := Reject("prov", req, FeatureReasoningSummary); err == nil {
		t.Fatal("expected error once summary is set")
	}
}

func TestRejectUnknownFeatureFailsLoud(t *testing.T) {
	if err := Reject("prov", agents.ModelRequest{}, Feature("bogus")); err == nil {
		t.Fatal("expected error for unknown feature name")
	}
}
