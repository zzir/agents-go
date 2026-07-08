package openai

import (
	"errors"
	"testing"

	"github.com/zzir/agents-go/agents"
)

func TestProvider_EmptyModelIsUserError(t *testing.T) {
	_, err := NewProvider().GetModel("")
	if err == nil {
		t.Fatal("GetModel(\"\") with no default should error")
	}
	var ue *agents.UserError
	if !errors.As(err, &ue) {
		t.Errorf("error = %T, want *agents.UserError", err)
	}
}

func TestProvider_ExplicitModelResolves(t *testing.T) {
	got, err := NewProvider().GetModel("gpt-4o")
	if err != nil {
		t.Fatal(err)
	}
	if rm := got.(*ResponsesModel); rm.model != "gpt-4o" {
		t.Errorf("resolved model = %q, want gpt-4o", rm.model)
	}
}

func TestProvider_WithDefaultModel(t *testing.T) {
	got, err := NewProvider().WithDefaultModel("gpt-4o-mini").GetModel("")
	if err != nil {
		t.Fatal(err)
	}
	if rm := got.(*ResponsesModel); rm.model != "gpt-4o-mini" {
		t.Errorf("resolved model = %q, want the provider default gpt-4o-mini", rm.model)
	}
}
