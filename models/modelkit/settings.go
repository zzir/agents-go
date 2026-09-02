package modelkit

import "github.com/zzir/agents-go/agents"

// Settings returns s by value, or the zero settings when s is nil, so an
// adapter reads fields without a nil check.
func Settings(s *agents.ModelSettings) agents.ModelSettings {
	if s == nil {
		return agents.ModelSettings{}
	}
	return *s
}

// ExtraOptions maps the settings' ExtraHeaders, ExtraQuery and ExtraBody onto
// a vendor SDK's per-request options; the three constructors are the SDK's
// WithHeader, WithQuery and WithJSONSet. body receives the key escaped as a
// literal sjson path, so a key with dots or brackets sets a top-level field.
func ExtraOptions[O any](s *agents.ModelSettings, header, query func(key, value string) O, body func(path string, value any) O) []O {
	if s == nil {
		return nil
	}
	var opts []O
	for k, v := range s.ExtraHeaders {
		opts = append(opts, header(k, v))
	}
	for k, v := range s.ExtraQuery {
		opts = append(opts, query(k, v))
	}
	for k, v := range s.ExtraBody {
		opts = append(opts, body(EscapeJSONPath(k), v))
	}
	return opts
}
