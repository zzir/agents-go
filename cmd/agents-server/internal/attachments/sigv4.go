package attachments

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// AWS Signature Version 4 for the three S3 operations this package performs.
// Implemented here rather than importing an SDK: the surface is one signing
// algorithm over three requests, and the AWS SDK would be the module's
// heaviest dependency by far.

const signAlgorithm = "AWS4-HMAC-SHA256"

// signV4 signs req in place: sets x-amz-date, x-amz-content-sha256, host and
// Authorization. payloadHash is hex(sha256(body)) — the empty-body hash for
// GET/DELETE.
func signV4(req *http.Request, accessKey, secretKey, region, service, payloadHash string, now time.Time) {
	amzDate := now.UTC().Format("20060102T150405Z")
	dateStamp := now.UTC().Format("20060102")

	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", payloadHash)
	req.Host = req.URL.Host

	canonicalHeaders, signedHeaders := canonicalizeHeaders(req)
	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURI(req.URL),
		canonicalQuery(req.URL),
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	scope := strings.Join([]string{dateStamp, region, service, "aws4_request"}, "/")
	stringToSign := strings.Join([]string{
		signAlgorithm,
		amzDate,
		scope,
		hexSHA256([]byte(canonicalRequest)),
	}, "\n")

	kDate := hmacSHA256([]byte("AWS4"+secretKey), dateStamp)
	kRegion := hmacSHA256(kDate, region)
	kService := hmacSHA256(kRegion, service)
	kSigning := hmacSHA256(kService, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(kSigning, stringToSign))

	req.Header.Set("Authorization", signAlgorithm+
		" Credential="+accessKey+"/"+scope+
		", SignedHeaders="+signedHeaders+
		", Signature="+signature)
}

// canonicalURI is the path, URI-encoded per sigv4 (every segment encoded,
// slashes kept). url.EscapedPath already preserves what Go parsed; S3 keys
// here are uuid-based ASCII, so the practical risk is nil — but encode
// defensively anyway.
func canonicalURI(u *url.URL) string {
	if u.Path == "" {
		return "/"
	}
	segs := strings.Split(u.Path, "/")
	for i, s := range segs {
		segs[i] = uriEncode(s)
	}
	return strings.Join(segs, "/")
}

// canonicalQuery sorts and encodes the query per sigv4.
func canonicalQuery(u *url.URL) string {
	q := u.Query()
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vs := q[k]
		sort.Strings(vs)
		for _, v := range vs {
			parts = append(parts, uriEncode(k)+"="+uriEncode(v))
		}
	}
	return strings.Join(parts, "&")
}

// uriEncode is the sigv4 variant of percent-encoding: unreserved characters
// (A-Z a-z 0-9 - . _ ~) stay literal, everything else is %XX uppercase.
func uriEncode(s string) string {
	var b strings.Builder
	for i := range len(s) {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '.', c == '_', c == '~':
			b.WriteByte(c)
		default:
			b.WriteString("%" + strings.ToUpper(hex.EncodeToString([]byte{c})))
		}
	}
	return b.String()
}

// canonicalizeHeaders returns the canonical headers block and the signed
// headers list: host plus every x-amz-* header, lowercased and sorted.
func canonicalizeHeaders(req *http.Request) (canonical, signed string) {
	headers := map[string]string{"host": req.Host}
	for k, vs := range req.Header {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "x-amz-") {
			headers[lk] = strings.TrimSpace(strings.Join(vs, ","))
		}
	}
	keys := make([]string, 0, len(headers))
	for k := range headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var c strings.Builder
	for _, k := range keys {
		c.WriteString(k + ":" + headers[k] + "\n")
	}
	return c.String(), strings.Join(keys, ";")
}

func hexSHA256(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}
