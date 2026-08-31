package e2b

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// The Connect protocol, by hand. Connect's JSON codec is what makes this
// possible without protobuf: a unary call is an ordinary JSON POST, and a
// server stream is the same JSON in a five-byte envelope. Generated stubs
// would buy type safety over a schema this package pins to six messages
// anyway, at the cost of a protobuf toolchain in the build — see
// decisions §5.34.
//
// Wire shape (connectrpc.com/connect, protocol v1):
//
//	unary   POST /<package>.<Service>/<Method>   Content-Type: application/json
//	stream  POST /<package>.<Service>/<Method>   Content-Type: application/connect+json
//	        body: repeated [1 flag byte][4-byte big-endian length][payload]
//	        the frame with flag bit 0x02 is the end-of-stream trailer
const (
	connectVersionHeader = "Connect-Protocol-Version"
	connectVersion       = "1"
	// endStreamFlag marks the trailer frame that closes a server stream.
	endStreamFlag = 0x02
	// maxFrameBytes bounds one envelope. envd's frames are single events;
	// anything larger is a malformed or hostile response, and reading it
	// would be an unbounded allocation driven by five header bytes.
	maxFrameBytes = 16 << 20
)

// connectError is an RPC failure as Connect reports it: a code that means
// something ("not_found") plus a message.
type connectError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *connectError) Error() string {
	if e.Message == "" {
		return "connect: " + e.Code
	}
	return "connect: " + e.Code + ": " + e.Message
}

// isNotFound reports the one code callers branch on — a missing path, a dead
// process — so it can be mapped to fs.ErrNotExist.
func isNotFound(err error) bool {
	var ce *connectError
	return errors.As(err, &ce) && ce.Code == "not_found"
}

// unary performs one request/response call and decodes the reply into out.
func (s *Sandbox) unary(ctx context.Context, procedure string, in, out any) error {
	base, err := s.envdBase(ctx)
	if err != nil {
		return err
	}
	return s.unaryAt(ctx, base, procedure, in, out)
}

// unaryAt is unary against an explicit base — see envdRequestAt.
func (s *Sandbox) unaryAt(ctx context.Context, base, procedure string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("e2b: encoding %s: %w", procedure, err)
	}
	req, err := s.envdRequestAt(ctx, base, http.MethodPost, procedure, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(connectVersionHeader, connectVersion)
	resp, err := s.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("e2b: %s: %w", procedure, err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, maxFrameBytes))
	if err != nil {
		return fmt.Errorf("e2b: %s: %w", procedure, err)
	}
	if resp.StatusCode != http.StatusOK {
		return decodeConnectError(resp.StatusCode, payload)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("e2b: decoding %s: %w", procedure, err)
	}
	return nil
}

// stream opens a server-streaming call and hands each message payload to fn.
// It returns when the stream ends; a trailer carrying an error returns it, so
// a caller cannot mistake a failed stream for an empty one.
func (s *Sandbox) stream(ctx context.Context, procedure string, in any, fn func(raw json.RawMessage) error) error {
	body, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("e2b: encoding %s: %w", procedure, err)
	}
	// Outgoing frames obey the same cap as inbound ones (readFrame): the length
	// rides a uint32 prefix, and a control request never approaches it.
	if len(body) > maxFrameBytes {
		return fmt.Errorf("e2b: %s: request of %d bytes exceeds the %d cap", procedure, len(body), maxFrameBytes)
	}
	req, err := s.envdRequest(ctx, http.MethodPost, procedure, bytes.NewReader(envelope(0, body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/connect+json")
	req.Header.Set(connectVersionHeader, connectVersion)
	resp, err := s.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("e2b: %s: %w", procedure, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(io.LimitReader(resp.Body, maxFrameBytes))
		return decodeConnectError(resp.StatusCode, payload)
	}
	for {
		flag, payload, err := readFrame(resp.Body)
		if err != nil {
			if errors.Is(err, io.EOF) {
				// A stream that ends without its trailer is a truncated
				// response, not a clean end: say so rather than reporting
				// whatever was collected as complete.
				return fmt.Errorf("e2b: %s: stream ended without a trailer", procedure)
			}
			return fmt.Errorf("e2b: %s: %w", procedure, err)
		}
		if flag&endStreamFlag != 0 {
			var trailer struct {
				Error *connectError `json:"error"`
			}
			if json.Unmarshal(payload, &trailer) == nil && trailer.Error != nil {
				return trailer.Error
			}
			return nil
		}
		if err := fn(payload); err != nil {
			return err
		}
	}
}

// envelope frames one payload for the streaming protocol.
func envelope(flag byte, payload []byte) []byte {
	out := make([]byte, 5+len(payload))
	out[0] = flag
	binary.BigEndian.PutUint32(out[1:5], uint32(len(payload)))
	copy(out[5:], payload)
	return out
}

// readFrame reads one envelope.
func readFrame(r io.Reader) (byte, []byte, error) {
	var head [5]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(head[1:5])
	if n > maxFrameBytes {
		return 0, nil, fmt.Errorf("frame of %d bytes exceeds the %d cap", n, maxFrameBytes)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		// A short read after a valid header is a truncated frame, never a
		// clean end — io.EOF here would read as one.
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return 0, nil, err
	}
	return head[0], payload, nil
}

// decodeConnectError turns a non-200 into a connectError, falling back to the
// status when the body is not the shape Connect promises.
func decodeConnectError(status int, payload []byte) error {
	var ce connectError
	if json.Unmarshal(payload, &ce) == nil && ce.Code != "" {
		return &ce
	}
	return &connectError{Code: fmt.Sprintf("http_%d", status), Message: capBody(string(bytes.TrimSpace(payload)))}
}
