package docker

import (
	"strings"
	"testing"
)

// The ssh:// URL carries everything the dialer needs: user (required), host
// (port defaults to 22), and optionally the remote socket path.
func TestNewSSHDialerParsesHostURL(t *testing.T) {
	auth := SSHAuth{Password: "pw"}
	cases := []struct {
		url        string
		addr, sock string
	}{
		{"ssh://u@h", "h:22", "/var/run/docker.sock"},
		{"ssh://u@h:2222", "h:2222", "/var/run/docker.sock"},
		{"ssh://u@h/run/user/1000/docker.sock", "h:22", "/run/user/1000/docker.sock"},
	}
	for _, tc := range cases {
		d, err := newSSHDialer(tc.url, auth)
		if err != nil {
			t.Fatalf("%s: %v", tc.url, err)
		}
		if d.addr != tc.addr || d.socket != tc.sock {
			t.Errorf("%s: (addr, socket) = (%q, %q), want (%q, %q)", tc.url, d.addr, d.socket, tc.addr, tc.sock)
		}
		if d.cfg.User != "u" {
			t.Errorf("%s: user = %q", tc.url, d.cfg.User)
		}
	}
}

func TestNewSSHDialerRefusesBadInput(t *testing.T) {
	if _, err := newSSHDialer("ssh://h", SSHAuth{Password: "pw"}); err == nil || !strings.Contains(err.Error(), "user") {
		t.Fatalf("userless URL: err = %v, want a user requirement", err)
	}
	if _, err := newSSHDialer("ssh://u@h", SSHAuth{}); err == nil || !strings.Contains(err.Error(), "authentication") {
		t.Fatalf("no auth method: err = %v, want an auth requirement", err)
	}
}
