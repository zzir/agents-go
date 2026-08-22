package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/uptrace/bun"

	"github.com/zzir/agents-go/cmd/agents-server/internal/secrets"
)

// secretBox seals credential columns at rest. Set once at startup by
// UseSecretBox, before any store is used; nil is plaintext mode (the
// single-user workbench, or a server whose operator has not set a key).
var secretBox *secrets.Box

// UseSecretBox installs the process's key for every store.
func UseSecretBox(b *secrets.Box) { secretBox = b }

// sealSecret seals plain for its place, "table.column" (a JSON field inside
// a column: "table.column.field") — the label the ciphertext is bound to.
func sealSecret(label, plain string) string { return secretBox.Seal(label, plain) }

func openSecret(label, stored string) (string, error) { return secretBox.Open(label, stored) }

// sealJSONKeys seals the named top-level string fields of a JSON object
// stored in the column named by label; a key whose value is an object (a
// headers map) has each string value sealed. Absent keys and non-string
// values are left alone.
func sealJSONKeys(label string, raw json.RawMessage, keys ...string) (json.RawMessage, error) {
	return mapJSONKeys(label, raw, func(l, s string) (string, error) { return sealSecret(l, s), nil }, keys...)
}

// openJSONKeys is sealJSONKeys's inverse.
func openJSONKeys(label string, raw json.RawMessage, keys ...string) (json.RawMessage, error) {
	return mapJSONKeys(label, raw, openSecret, keys...)
}

func mapJSONKeys(label string, raw json.RawMessage, fn func(label, s string) (string, error), keys ...string) (json.RawMessage, error) {
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
			out, err := fn(label+"."+k, str)
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
				out, err := fn(label+"."+k, mv)
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
	return strings.Contains(string(raw), `"enc:`)
}

// secretKeyCheck is the settings row that proves the configured key is the
// one the database's secrets were sealed under: a canary sealed at the
// first start with a key, opened at every start after.
const secretKeyCheck = "secret_key_check"

// VerifySecretKey fails fast, at startup, on a key the database's sealed
// rows would not open: a sealed canary with no key configured, or with
// another key — either would otherwise surface as the first settings
// panel failing to load. With a key and no canary yet, it seals one.
func VerifySecretKey(ctx context.Context, db *bun.DB) error {
	var stored string
	err := db.NewSelect().Model((*Setting)(nil)).Column("value").Where("key = ?", secretKeyCheck).Scan(ctx, &stored)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if secretBox == nil {
			return nil
		}
		st := &Setting{Key: secretKeyCheck, Value: sealSecret(labelSetting, "ok")}
		if _, err := db.NewInsert().Model(st).On("CONFLICT (key) DO NOTHING").Exec(ctx); err != nil {
			return fmt.Errorf("sealing the secret key check: %w", err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("reading the secret key check: %w", err)
	}
	if secretBox == nil {
		return errors.New("this database holds credentials sealed under a key, and none is configured: set AGENTS_SECRET_KEY (or --secret-key-file) to the key they were sealed with")
	}
	if _, err := openSecret(labelSetting, stored); err != nil {
		return fmt.Errorf("the configured secret key is not the one this database's credentials were sealed under (%w); restore that key, or start with a fresh database", err)
	}
	return nil
}
