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
		{"docker ssh host kept", "docker", `{"image":"i","host":"ssh://u@h"}`,
			`{"image":"i","host":"ssh://u@h","network":false,"persistent":false}`, ""},
		{"docker tcp host kept", "docker", `{"image":"i","host":"tcp://h:2375"}`,
			`{"image":"i","host":"tcp://h:2375","network":false,"persistent":false}`, ""},
		{"docker ssh host without user refused", "docker", `{"image":"i","host":"ssh://h"}`,
			"", "must carry its user"},
		{"docker bare host refused", "docker", `{"image":"i","host":"remote:2375"}`,
			"", "host must be empty"},
		{"docker negative memory refused", "docker", `{"image":"i","memory_mb":-1}`,
			"", "cannot be negative"},
		{"docker negative cap refused", "docker", `{"image":"i","max_read_file_bytes":-1}`,
			"", "cannot be negative"},
		{"non-docker type refused", "local", `{}`, "", "must be docker"},
		{"ssh type refused", "ssh", `{"addr":"h","user":"u"}`, "", "must be docker"},
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
		a, b string
		want bool
	}{
		// The load-bearing case: the UI round-trips every field with explicit
		// zeros, the raw API writes only the non-zero ones.
		{"omitted vs explicit zero fields",
			`{"image":"i"}`,
			`{"image":"i","host":"","runtime":"","user":"","network":false,"memory_mb":0,"cpus":0,"persistent":false}`,
			true},
		{"key order and whitespace",
			`{"image":"i","persistent":true}`, `{ "persistent": true, "image": "i" }`, true},
		{"host dir trailing slash",
			`{"image":"i","host_dir":"/data/"}`, `{"image":"i","host_dir":"/data"}`, true},
		{"unknown key ignored",
			`{"image":"i"}`, `{"image":"i","future_field":1}`, true},
		{"network flip is a change",
			`{"image":"i","network":false}`, `{"image":"i","network":true}`, false},
		{"ssh credential change is a change",
			`{"image":"i","host":"ssh://u@h","ssh_password":"old"}`,
			`{"image":"i","host":"ssh://u@h","ssh_password":"new"}`, false},
		{"memory cap change is a change",
			`{"image":"i","memory_mb":512}`, `{"image":"i","memory_mb":1024}`, false},
		// A payload that cannot decode compares unequal — the caller retires
		// (rebuilds an environment) rather than risking old credentials
		// serving on.
		{"undecodable compares unequal", `{"image":"i"}`, `{"image":42}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ContentEqual("docker", json.RawMessage(tc.a), json.RawMessage(tc.b)); got != tc.want {
				t.Fatalf("ContentEqual(%s, %s) = %v, want %v", tc.a, tc.b, got, tc.want)
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
		{"type change", sb("docker", `{"image":"i"}`), sb("future", `{"image":"i"}`), true},
		{"daemon move", sb("docker", `{"image":"i"}`), sb("docker", `{"image":"i","host":"ssh://u@h"}`), true},
		{"ssh credential rotation is not identity",
			sb("docker", `{"image":"i","host":"ssh://u@h","ssh_password":"old"}`),
			sb("docker", `{"image":"i","host":"ssh://u@h","ssh_password":"new"}`), false},
		{"host dir slash spelling", sb("docker", `{"image":"i","host_dir":"/d/"}`), sb("docker", `{"image":"i","host_dir":"/d"}`), false},
		{"persistent flip", sb("docker", `{"image":"i"}`), sb("docker", `{"image":"i","persistent":true}`), true},
		{"image swap is not identity", sb("docker", `{"image":"a"}`), sb("docker", `{"image":"b"}`), false},
		{"limits change is not identity", sb("docker", `{"image":"i"}`), sb("docker", `{"image":"i","memory_mb":512,"cpus":2}`), false},
		// A stored config that no longer decodes cannot be moved OFF of
		// without this reading false: fixing it is the bound sessions' only
		// way out.
		{"undecodable prev allows the repair", sb("docker", `{"image":"i","persistent":"yes"}`), sb("docker", `{"image":"i","persistent":true}`), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IdentityChanged(tc.prev, tc.next); got != tc.want {
				t.Fatalf("IdentityChanged = %v, want %v", got, tc.want)
			}
		})
	}
}
