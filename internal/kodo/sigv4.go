// Package kodo stores recordings in Qiniu Kodo through its S3-compatible
// API, using a minimal AWS Signature V4 implementation (no SDK dependency).
package kodo

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	algorithm       = "AWS4-HMAC-SHA256"
	unsignedPayload = "UNSIGNED-PAYLOAD"
	amzDate         = "20060102T150405Z"
)

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// uriEncode implements S3's URI encoding: unreserved characters are kept,
// everything else is %XX; "/" is kept in paths.
func uriEncode(s string, keepSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z', '0' <= c && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && keepSlash:
			b.WriteByte(c)
		default:
			b.WriteString("%" + strings.ToUpper(hex.EncodeToString([]byte{c})))
		}
	}
	return b.String()
}

func canonicalQuery(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vals := append([]string(nil), q[k]...)
		sort.Strings(vals)
		for _, v := range vals {
			parts = append(parts, uriEncode(k, false)+"="+uriEncode(v, false))
		}
	}
	return strings.Join(parts, "&")
}

type signer struct {
	accessKey, secretKey, region string
}

func (s signer) scope(t time.Time) string {
	return t.Format("20060102") + "/" + s.region + "/s3/aws4_request"
}

func (s signer) signature(t time.Time, canonicalRequest string) string {
	stringToSign := strings.Join([]string{
		algorithm, t.Format(amzDate), s.scope(t), sha256Hex([]byte(canonicalRequest)),
	}, "\n")
	k := hmacSHA256([]byte("AWS4"+s.secretKey), t.Format("20060102"))
	k = hmacSHA256(k, s.region)
	k = hmacSHA256(k, "s3")
	k = hmacSHA256(k, "aws4_request")
	return hex.EncodeToString(hmacSHA256(k, stringToSign))
}

// sign adds SigV4 Authorization headers to req. payloadHash is the hex
// SHA-256 of the body (or UNSIGNED-PAYLOAD).
func (s signer) sign(req *http.Request, payloadHash string, t time.Time) {
	s.signWith(req, payloadHash, t)
}

// signWith signs Content-Type, Host, x-amz-* and any extra headers named.
func (s signer) signWith(req *http.Request, payloadHash string, t time.Time, extra ...string) {
	t = t.UTC()
	req.Header.Set("X-Amz-Date", t.Format(amzDate))
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	host := req.URL.Host
	headers := map[string]string{"host": host}
	for k, v := range req.Header {
		lk := strings.ToLower(k)
		if lk == "content-type" || strings.HasPrefix(lk, "x-amz-") || slices.Contains(extra, lk) {
			headers[lk] = strings.TrimSpace(strings.Join(v, ","))
		}
	}
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)
	var canonHeaders strings.Builder
	for _, k := range names {
		canonHeaders.WriteString(k + ":" + headers[k] + "\n")
	}
	signed := strings.Join(names, ";")
	cr := strings.Join([]string{
		req.Method, uriEncode(req.URL.Path, true), canonicalQuery(req.URL.Query()),
		canonHeaders.String(), signed, payloadHash,
	}, "\n")
	req.Header.Set("Authorization", algorithm+" Credential="+s.accessKey+"/"+s.scope(t)+
		", SignedHeaders="+signed+", Signature="+s.signature(t, cr))
}

// presign returns a query-signed URL valid for expires.
func (s signer) presign(method string, u *url.URL, expires time.Duration, t time.Time) string {
	t = t.UTC()
	q := u.Query()
	q.Set("X-Amz-Algorithm", algorithm)
	q.Set("X-Amz-Credential", s.accessKey+"/"+s.scope(t))
	q.Set("X-Amz-Date", t.Format(amzDate))
	q.Set("X-Amz-Expires", strconv.Itoa(int(expires/time.Second)))
	q.Set("X-Amz-SignedHeaders", "host")
	cr := strings.Join([]string{
		method, uriEncode(u.Path, true), canonicalQuery(q),
		"host:" + u.Host + "\n", "host", unsignedPayload,
	}, "\n")
	q.Set("X-Amz-Signature", s.signature(t, cr))
	out := *u
	out.RawQuery = canonicalQuery(q)
	return out.String()
}
