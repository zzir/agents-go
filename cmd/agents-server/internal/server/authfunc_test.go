package server

import (
	"context"
	"errors"

	"github.com/zzir/agents-go/cmd/agents-server/internal/protocol"
)

// staticAuth is the tests' stand-in for authn's token mode: one credential,
// one implied user.
func staticAuth(tok string) AuthFunc {
	return func(_ context.Context, bearer string) (protocol.UserInfo, error) {
		if bearer != tok {
			return protocol.UserInfo{}, errors.New("unauthorized")
		}
		return protocol.UserInfo{ID: "local", Email: "local@localhost", Role: "admin"}, nil
	}
}
