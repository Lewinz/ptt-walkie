package kodo

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// Test vector from the AWS S3 docs ("Authenticating Requests: Using Query
// Parameters", example GET of examplebucket/test.txt).
func TestPresignAWSVector(t *testing.T) {
	s := signer{accessKey: "AKIAIOSFODNN7EXAMPLE", secretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", region: "us-east-1"}
	u, _ := url.Parse("https://examplebucket.s3.amazonaws.com/test.txt")
	at := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)
	got := s.presign("GET", u, 86400*time.Second, at)
	want := "aeeed9bbccd4d02ee5c0109b86d86835f995330da4c265957d157751f604d404"
	if !strings.Contains(got, "X-Amz-Signature="+want) {
		t.Fatalf("presign = %s", got)
	}
}

// Test vector from the AWS S3 docs ("Signature Calculations for the
// Authorization Header", GET object with a Range header).
func TestHeaderSignAWSVector(t *testing.T) {
	s := signer{accessKey: "AKIAIOSFODNN7EXAMPLE", secretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", region: "us-east-1"}
	req, _ := http.NewRequest("GET", "https://examplebucket.s3.amazonaws.com/test.txt", nil)
	req.Header.Set("Range", "bytes=0-9")
	// Range is signed in the AWS example; include it via an x-amz-style hook.
	at := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)
	emptyHash := sha256Hex(nil)
	s.signWith(req, emptyHash, at, "range")
	want := "f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	if got := req.Header.Get("Authorization"); !strings.HasSuffix(got, "Signature="+want) {
		t.Fatalf("authorization = %s", got)
	}
}

func TestPutAndPresignAgainstFakeKodo(t *testing.T) {
	var gotPath, gotType, gotAuth string
	var gotBody []byte
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotType, gotAuth = r.URL.Path, r.Header.Get("Content-Type"), r.Header.Get("Authorization")
		gotBody, _ = io.ReadAll(r.Body)
		if r.Header.Get("X-Amz-Content-Sha256") != sha256Hex(gotBody) {
			http.Error(w, "bad payload hash", 400)
			return
		}
	}))
	defer fake.Close()
	c := &Client{Endpoint: fake.URL, Region: "cn-east-1", Bucket: "ptt", AccessKey: "ak", SecretKey: "sk"}
	if err := c.Put(context.Background(), "recordings/g1/u1.ogg", "audio/ogg", []byte("OggS...")); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/ptt/recordings/g1/u1.ogg" || gotType != "audio/ogg" || string(gotBody) != "OggS..." {
		t.Fatalf("put %s %s %q", gotPath, gotType, gotBody)
	}
	if !strings.Contains(gotAuth, "Credential=ak/") || !strings.Contains(gotAuth, "/cn-east-1/s3/aws4_request") ||
		!strings.Contains(gotAuth, "SignedHeaders=content-type;host;x-amz-content-sha256;x-amz-date") {
		t.Fatalf("auth %s", gotAuth)
	}
	link, _ := c.PresignGet("recordings/g1/u1.ogg", time.Hour)
	if !strings.HasPrefix(link, fake.URL+"/ptt/recordings/g1/u1.ogg?") || !strings.Contains(link, "X-Amz-Expires=3600") {
		t.Fatalf("presign %s", link)
	}
}
