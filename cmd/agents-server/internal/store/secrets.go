package store

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/zzir/agents-go/cmd/agents-server/internal/secrets"
)

// secretBox seals credential columns at rest. Set once at startup by
// UseSecretBox, before any store is used; nil is plaintext mode (the
// single-user workbench, or a server whose operator has not set a key).
var secretBox *secrets.Box

// UseSecretBox installs the process's key for every store.
func UseSecretBox(b *secrets.Box) { secretBox = b }

func sealSecret(plain string) string { return secretBox.Seal(plain) }

func openSecret(stored string) (string, error) { return secretBox.Open(stored) }

// sealJSONKeys seals the named top-level string fields of a JSON object; a
// key whose value is an object (a headers map) has each string value sealed.
// Absent keys and non-string values are left alone.
func sealJSONKeys(raw json.RawMessage, keys ...string) (json.RawMessage, error) {
	return mapJSONKeys(raw, func(s string) (string, error) { return sealSecret(s), nil }, keys...)
}

// openJSONKeys is sealJSONKeys's inverse.
func openJSONKeys(raw json.RawMessage, keys ...string) (json.RawMessage, error) {
	return mapJSONKeys(raw, openSecret, keys...)
}

func mapJSONKeys(raw json.RawMessage, fn func(string) (string, error), keys ...string) (json.RawMessage, error) {
	if len(raw) == 0 || secretBox == nil && !hasSealed(raw) {
		return raw, nil
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil || obj == nil {
		return raw, nil //nolint:nilerr // not an object: nothing here to seal, the value passes through
	}
	changed := false
	for _, k := range keys {
		v, ok := obj[k]
		if !ok {
			continue
		}
		var str string
		if err := json.Unmarshal(v, &str); err == nil {
			out, err := fn(str)
			if err != nil {
				return nil, fmt.Errorf("field %q: %w", k, err)
			}
			obj[k], _ = json.Marshal(out)
			changed = true
			continue
		}
		var m map[string]string
		if err := json.Unmarshal(v, &m); err == nil && m != nil {
			for mk, mv := range m {
				out, err := fn(mv)
				if err != nil {
					return nil, fmt.Errorf("field %q.%s: %w", k, mk, err)
				}
				m[mk] = out
			}
			obj[k], _ = json.Marshal(m)
			changed = true
		}
	}
	if !changed {
		return raw, nil
	}
	return json.Marshal(obj)
}

// hasSealed reports whether raw contains a sealed value — so plaintext mode
// still opens (and fails loudly on) rows sealed under a key that is gone.
func hasSealed(raw json.RawMessage) bool {
	return strings.Contains(string(raw), `"enc:v1:`)
}
