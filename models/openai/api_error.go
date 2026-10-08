package openai

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"

	oai "github.com/openai/openai-go/v3"
)

// errorBodyCap bounds how much of a body with no error object (an HTML gateway
// page) goes into the error text.
const errorBodyCap = 2048

// withErrorDetail puts back what openai-go leaves out of an API error's text:
// the endpoint, without query string or userinfo, and the provider's error
// object, or the body when it has none — see spec §2.15.
func withErrorDetail(err error) error {
	e, ok := errors.AsType[*oai.Error](err)
	if !ok {
		return err
	}
	var detail strings.Builder
	if e.Request != nil && e.Request.URL != nil {
		u := *e.Request.URL
		u.User, u.RawQuery, u.Fragment = nil, "", ""
		fmt.Fprintf(&detail, " (%s %s)", e.Request.Method, u.String())
	}
	if body := errorBody(e); body != "" {
		detail.WriteString(" " + body)
	}
	if detail.Len() == 0 {
		return err
	}
	return &detailedError{err: err, detail: detail.String()}
}

// errorBody is the provider's error object, or the response body when it has
// none; the body is left readable for DumpResponse.
func errorBody(e *oai.Error) string {
	if raw := e.RawJSON(); raw != "" {
		return raw
	}
	if e.Response == nil || e.Response.Body == nil {
		return ""
	}
	contents, _ := io.ReadAll(e.Response.Body)
	e.Response.Body = io.NopCloser(bytes.NewReader(contents))
	body := strings.TrimSpace(string(contents))
	if len(body) > errorBodyCap {
		body = strings.ToValidUTF8(body[:errorBodyCap], "") + "…"
	}
	return body
}

// detailedError is err with the endpoint and error body appended to its text.
type detailedError struct {
	err    error
	detail string
}

func (e *detailedError) Error() string { return e.err.Error() + e.detail }
func (e *detailedError) Unwrap() error { return e.err }
