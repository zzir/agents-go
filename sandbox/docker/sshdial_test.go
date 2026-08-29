package docker

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// The ssh:// URL carries everything the dialer needs: user (required), host
// (port defaults to 22), and optionally the remote socket path.
func TestNewSSHDialerParsesHostURL(t *testing.T) {
	// Parsing is what is under test, so host-key verification is switched off:
	// its default reads ~/.ssh/known_hosts, which would make this depend on
	// whether the machine running it has ever used SSH.
	auth := SSHAuth{Password: "pw", InsecureIgnoreHostKey: true}
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
	auth := SSHAuth{Password: "pw", InsecureIgnoreHostKey: true} // parsing only; see above
	d, err := newSSHDialer("ssh://u@[::1]", auth)
	if err != nil {
		t.Fatal(err)
	}
	if d.addr != "[::1]:22" {
		t.Errorf("ipv6 addr = %q, want %q", d.addr, "[::1]:22")
	}
	if _, err := newSSHDialer("ssh://u@", auth); err == nil {
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

// startRejectingSSHD runs a minimal sshd that completes handshakes (counting
// them) but rejects every channel open — a healthy transport whose target
// port is not listening.
func startRejectingSSHD(t *testing.T) (addr string, handshakes *atomic.Int32) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(ssh.ConnMetadata, []byte) (*ssh.Permissions, error) { return nil, nil },
	}
	cfg.AddHostKey(signer)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	count := &atomic.Int32{}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				sconn, chans, reqs, err := ssh.NewServerConn(c, cfg)
				if err != nil {
					_ = c.Close()
					return
				}
				count.Add(1)
				go ssh.DiscardRequests(reqs)
				for nc := range chans {
					_ = nc.Reject(ssh.ConnectionFailed, "port not listening")
				}
				_ = sconn.Close()
			}()
		}
	}()
	return ln.Addr().String(), count
}

// A refused direct-tcpip channel rides a HEALTHY transport (the container port
// is not listening yet): dialThrough must surface it without reconnecting, or
// the teardown severs every in-flight stream multiplexed on the shared client.
// A transport-level failure still reconnects.
func TestDialThroughChannelRejectionKeepsTransport(t *testing.T) {
	addr, handshakes := startRejectingSSHD(t)
	d, err := newSSHDialer("ssh://u@"+addr, SSHAuth{
		Password:              "pw",
		InsecureIgnoreHostKey: true,
		ConnectTimeout:        5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	ctx := t.Context()
	for range 2 {
		_, err := d.dialThrough(ctx, "tcp", "127.0.0.1:1")
		if _, ok := errors.AsType[*ssh.OpenChannelError](err); !ok {
			t.Fatalf("dialThrough err = %v, want the channel rejection", err)
		}
	}
	if n := handshakes.Load(); n != 1 {
		t.Fatalf("handshakes = %d, want 1: a rejected channel must not tear down the transport", n)
	}

	// Severing the transport is the case that DOES reconnect.
	d.mu.Lock()
	c := d.client
	d.mu.Unlock()
	_ = c.Close()
	if _, err := d.dialThrough(ctx, "tcp", "127.0.0.1:1"); err == nil {
		t.Fatal("dialThrough after a severed transport: want the channel rejection, got nil")
	}
	if n := handshakes.Load(); n != 2 {
		t.Fatalf("handshakes = %d, want 2: a severed transport must reconnect", n)
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
