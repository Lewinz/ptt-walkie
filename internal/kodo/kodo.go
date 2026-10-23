package kodo

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client is a minimal S3 client for Qiniu Kodo
// (endpoint https://s3.{region}.qiniucs.com, path-style addressing).
type Client struct {
	Endpoint  string // e.g. https://s3.cn-east-1.qiniucs.com
	Region    string // e.g. cn-east-1
	Bucket    string
	AccessKey string
	SecretKey string
	HTTP      *http.Client

	now func() time.Time
}

func (c *Client) signer() signer {
	return signer{accessKey: c.AccessKey, secretKey: c.SecretKey, region: c.Region}
}

func (c *Client) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

func (c *Client) objectURL(key string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimRight(c.Endpoint, "/"))
	if err != nil {
		return nil, err
	}
	u.Path = "/" + c.Bucket + "/" + strings.TrimLeft(key, "/")
	return u, nil
}

// Put uploads an object.
func (c *Client) Put(ctx context.Context, key, contentType string, body []byte) error {
	u, err := c.objectURL(key)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	req.ContentLength = int64(len(body))
	c.signer().sign(req, sha256Hex(body), c.clock())
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("kodo put %s: %s %s", key, resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}

// PresignGet returns a time-limited download URL for a private object.
func (c *Client) PresignGet(key string, expires time.Duration) (string, error) {
	u, err := c.objectURL(key)
	if err != nil {
		return "", err
	}
	return c.signer().presign(http.MethodGet, u, expires, c.clock()), nil
}
