package store

import (
	"encoding/json"
	"strings"
	"testing"
)

// NormalizeSandboxConfig is the API write gate: what it admits must build
// later, and what it returns is the canonical stored form.
func TestNormalizeSandboxConfig(t *testing.T) {
	cases := []struct {
		name    string
		typ     string
		in      string
		want    string // "" = expect an error
		wantErr string
	}{
		{"docker minimal", "docker", `{"image":"i"}`,
			`{"image":"i","network":false,"persistent":false}`, ""},
		{"docker type mismatch refused", "docker", `{"image":"i","persistent":"yes"}`,
			"", "docker config"},
		{"docker missing image", "docker", `{"persistent":true}`,
			"", "requires config.image"},
		{"docker host dir cleaned", "docker", `{"image":"i","persistent":true,"host_dir":"/data/proj/"}`,
			`{"image":"i","network":false,"persistent":true,"host_dir":"/data/proj"}`, ""},
		{"docker unknown key dropped", "docker", `{"image":"i","host":"remote"}`,
			`{"image":"i","network":false,"persistent":false}`, ""},
		{"ssh addr gets default port", "ssh", `{"addr":"h","user":"u"}`,
			`{"addr":"h:22","user":"u","use_agent":false,"insecure_host_key":false}`, ""},
		{"ssh explicit port kept", "ssh", `{"addr":"h:2222","user":"u"}`,
			`{"addr":"h:2222","user":"u","use_agent":false,"insecure_host_key":false}`, ""},
		{"ssh work dir cleaned", "ssh", `{"addr":"h:22","user":"u","work_dir":"/srv/app/"}`,
			`{"addr":"h:22","user":"u","use_agent":false,"insecure_host_key":false,"work_dir":"/srv/app"}`, ""},
		{"ssh missing user", "ssh", `{"addr":"h"}`, "", "requires config.user"},
		{"ssh missing addr", "ssh", `{"user":"u"}`, "", "requires config.addr"},
		{"ssh type mismatch refused", "ssh", `{"addr":"h","user":42}`, "", "ssh config"},
		{"local empty stays empty", "local", ``, ``, ""},
		{"local negative cap refused", "local", `{"max_read_file_bytes":-1}`,
			"", "cannot be negative"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeSandboxConfig(tc.typ, json.RawMessage(tc.in))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Fatalf("canonical = %s, want %s", got, tc.want)
			}
		})
	}
}

// ContentEqual is the predicate that decides whether an update retires live
// instances and severs terminals — representation noise must never trip it,
// and real changes must never slip past it.
func TestContentEqual(t *testing.T) {
	cases := []struct {
		name string
		typ  string
		a, b string
		want bool
	}{
		// The load-bearing case: the UI round-trips every field with explicit
		// zeros, the raw API writes only the non-zero ones.
		{"ssh omitted vs explicit zero fields", "ssh",
			`{"addr":"h","user":"u","work_dir":"/srv"}`,
			`{"addr":"h","user":"u","use_agent":false,"key_file":"","password":"","known_hosts":"","insecure_host_key":false,"work_dir":"/srv"}`,
			true},
		// Canonical spellings of one target: a stored config predating
		// canonicalization must compare equal to its canonical echo.
		{"ssh implicit vs explicit default port", "ssh",
			`{"addr":"h","user":"u"}`, `{"addr":"h:22","user":"u"}`, true},
		{"ssh trailing slash workdir", "ssh",
			`{"addr":"h","user":"u","work_dir":"/srv/app/"}`,
			`{"addr":"h:22","user":"u","work_dir":"/srv/app"}`, true},
		{"ssh real change", "ssh", `{"addr":"h"}`, `{"addr":"h2"}`, false},
		{"ssh port change is real", "ssh",
			`{"addr":"h:22","user":"u"}`, `{"addr":"h:2222","user":"u"}`, false},
		{"ssh credential flip is a change", "ssh",
			`{"addr":"h","use_agent":false}`, `{"addr":"h","use_agent":true}`, false},
		{"docker key order and whitespace", "docker",
			`{"image":"i","persistent":true}`, `{ "persistent": true, "image": "i" }`, true},
		{"docker host dir trailing slash", "docker",
			`{"image":"i","host_dir":"/data/"}`, `{"image":"i","host_dir":"/data"}`, true},
		{"docker unknown key ignored", "docker",
			`{"image":"i"}`, `{"image":"i","future_field":1}`, true},
		{"docker network flip is a change", "docker",
			`{"image":"i","network":false}`, `{"image":"i","network":true}`, false},
		{"local absent vs empty object", "local", ``, `{}`, true},
		{"local cap change", "local",
			`{"max_read_file_bytes":1}`, `{"max_read_file_bytes":2}`, false},
		// A payload the type cannot decode compares unequal — the caller
		// retires (rebuilds an environment) rather than risking old
		// credentials serving on.
		{"undecodable compares unequal", "ssh", `{"addr":"h"}`, `{"addr":42}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ContentEqual(tc.typ, json.RawMessage(tc.a), json.RawMessage(tc.b)); got != tc.want {
				t.Fatalf("ContentEqual(%s, %s, %s) = %v, want %v", tc.typ, tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// IdentityChanged guards the freeze on referenced sandboxes: spelling noise
// must not read as a move, and an undecodable stored config must not block
// its own repair.
func TestIdentityChanged(t *testing.T) {
	sb := func(typ, cfg string) *SandboxConfig {
		c := &SandboxConfig{Type: typ}
		if cfg != "" {
			c.Config = json.RawMessage(cfg)
		}
		return c
	}
	cases := []struct {
		name       string
		prev, next *SandboxConfig
		want       bool
	}{
		{"type change", sb("docker", `{"image":"i"}`), sb("ssh", `{"addr":"h","user":"u"}`), true},
		{"ssh addr default port spelling", sb("ssh", `{"addr":"h","user":"u"}`), sb("ssh", `{"addr":"h:22","user":"u"}`), false},
		{"ssh workdir trailing slash", sb("ssh", `{"addr":"h","user":"u","work_dir":"/srv/x/"}`), sb("ssh", `{"addr":"h:22","user":"u","work_dir":"/srv/x"}`), false},
		{"ssh machine move", sb("ssh", `{"addr":"h:22","user":"u"}`), sb("ssh", `{"addr":"h2:22","user":"u"}`), true},
		{"ssh user move", sb("ssh", `{"addr":"h:22","user":"a"}`), sb("ssh", `{"addr":"h:22","user":"b"}`), true},
		{"ssh credential rotation is not identity", sb("ssh", `{"addr":"h:22","user":"u","password":"old"}`), sb("ssh", `{"addr":"h:22","user":"u","password":"new"}`), false},
		{"docker host dir slash spelling", sb("docker", `{"image":"i","host_dir":"/d/"}`), sb("docker", `{"image":"i","host_dir":"/d"}`), false},
		{"docker persistent flip", sb("docker", `{"image":"i"}`), sb("docker", `{"image":"i","persistent":true}`), true},
		{"docker image swap is not identity", sb("docker", `{"image":"a"}`), sb("docker", `{"image":"b"}`), false},
		// A stored config that no longer decodes cannot be moved OFF of
		// without this reading false: fixing it is the bound sessions' only
		// way out.
		{"undecodable prev allows the repair", sb("docker", `{"image":"i","persistent":"yes"}`), sb("docker", `{"image":"i","persistent":true}`), false},
		{"local never has identity", sb("local", ``), sb("local", `{"max_read_file_bytes":1}`), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IdentityChanged(tc.prev, tc.next); got != tc.want {
				t.Fatalf("IdentityChanged = %v, want %v", got, tc.want)
			}
		})
	}
}
