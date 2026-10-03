package bridge

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	anthropicsdk "github.com/anthropics/anthropic-sdk-go"
	openaisdk "github.com/openai/openai-go/v3"

	"github.com/zzir/agents-go/agents"
	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
)

// A segment failure the SDK left unclassified is named by what the workbench
// can tell: the context overflowed, or a provider answered an HTTP error;
// anything else keeps the transport fallback, and an SDK code stands as is.
func TestRunErrorClassifiesProviderAndOverflow(t *testing.T) {
	// The SDK error types dereference their request and response in Error().
	req := httptest.NewRequest(http.MethodPost, "https://api.example.test/v1/responses", nil)
	resp := &http.Response{StatusCode: http.StatusUnauthorized}
	openaiErr := &openaisdk.Error{Request: req, Response: resp}
	anthropicErr := &anthropicsdk.Error{Request: req, Response: resp}

	cases := []struct {
		name     string
		err      error
		fallback string
		want     string
	}{
		{"openai http error, wrapped", fmt.Errorf("openai: %w", openaiErr), protocol.CodeStreamError, protocol.CodeProviderError},
		{"anthropic http error, on a resume", fmt.Errorf("anthropic: %w", anthropicErr), protocol.CodeResumeError, protocol.CodeProviderError},
		{"overflow by message", errors.New("400: This model's maximum context length is 128000 tokens"), protocol.CodeStreamError, protocol.CodeContextOverflow},
		{"overflow outranks the provider shape", fmt.Errorf("prompt is too long: %w", anthropicErr), protocol.CodeStreamError, protocol.CodeContextOverflow},
		{"plain transport failure", errors.New("read tcp: connection reset"), protocol.CodeStreamError, protocol.CodeStreamError},
		{"a provider error outside a segment keeps the fallback", fmt.Errorf("x: %w", openaiErr), protocol.CodeConfigError, protocol.CodeConfigError},
		{"an SDK code wins", &agents.MaxTurnsError{MaxTurns: 3}, protocol.CodeStreamError, string(agents.CodeMaxTurns)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := runErrorFor("r1", tc.err, tc.fallback).Code; got != tc.want {
				t.Fatalf("code = %q, want %q", got, tc.want)
			}
		})
	}
}
