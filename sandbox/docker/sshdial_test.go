package docker

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
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
		if d.user != "u" {
			t.Errorf("%s: user = %q", tc.url, d.user)
		}
	}
}

// A bracketed IPv6 host must produce a dialable host:port; an empty host is
// refused rather than silently dialing localhost.
func TestNewSSHDialerHostEdgeCases(t *testing.T) {
	d, err := newSSHDialer("ssh://u@[::1]", SSHAuth{Password: "pw"})
	if err != nil {
		t.Fatal(err)
	}
	if d.addr != "[::1]:22" {
		t.Errorf("ipv6 addr = %q, want %q", d.addr, "[::1]:22")
	}
	if _, err := newSSHDialer("ssh://u@", SSHAuth{Password: "pw"}); err == nil {
		t.Fatal("empty host must be refused")
	}
}

// A remote that accepts TCP but never speaks SSH must fail within
// ConnectTimeout — not hang the handshake (and connect's mutex) forever.
func TestSSHDialTimeoutCoversHandshake(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			defer conn.Close() // hold open, say nothing
		}
	}()

	d, err := newSSHDialer("ssh://u@"+ln.Addr().String(), SSHAuth{
		Password:              "pw",
		InsecureIgnoreHostKey: true,
		ConnectTimeout:        200 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := d.connect(context.Background(), nil); err == nil {
		t.Fatal("connect succeeded against a mute server")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("connect took %v, want ~ConnectTimeout", elapsed)
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
