package mcp

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// Endpoint is an outbound MCP connection in the xiaozhi / Lingxi
// "MCP 接入点" style: we dial the platform's WebSocket and serve JSON-RPC
// requests arriving over it, so the server needs no public address.
type Endpoint struct {
	URL      string
	DeviceID string // device the agent behind this endpoint speaks for
}

// ParseEndpoints parses "url|device_id; url2|device_id2" (device optional).
// A device_id query parameter in the URL is honoured too.
func ParseEndpoints(s string) []Endpoint {
	var out []Endpoint
	for _, part := range strings.FieldsFunc(s, func(r rune) bool { return r == ';' || r == '\n' }) {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		ep := Endpoint{URL: part}
		if i := strings.LastIndex(part, "|"); i >= 0 {
			ep.URL, ep.DeviceID = strings.TrimSpace(part[:i]), strings.TrimSpace(part[i+1:])
		}
		if ep.DeviceID == "" {
			if u, err := url.Parse(ep.URL); err == nil {
				ep.DeviceID = u.Query().Get("device_id")
			}
		}
		out = append(out, ep)
	}
	return out
}

// RunBridge keeps a connection to ep alive until ctx is cancelled,
// reconnecting with jittered exponential backoff.
func (s *Server) RunBridge(ctx context.Context, ep Endpoint) {
	backoff := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := s.serveEndpoint(ctx, ep)
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		wait := backoff + time.Duration(rand.Int64N(int64(backoff/2)+1))
		slog.Warn("mcp endpoint disconnected", "endpoint", redact(ep.URL), "err", err, "retry_in", wait.Round(time.Millisecond))
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		backoff = min(backoff*2, 2*time.Minute)
	}
}

func (s *Server) serveEndpoint(ctx context.Context, ep Endpoint) error {
	dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	ws, _, err := websocket.Dial(dctx, ep.URL, nil)
	cancel()
	if err != nil {
		return err
	}
	defer ws.CloseNow()
	ws.SetReadLimit(1 << 20)
	slog.Info("mcp endpoint connected", "endpoint", redact(ep.URL), "device", ep.DeviceID)

	// Keepalive: some platforms drop idle sockets after ~60s.
	go func() {
		t := time.NewTicker(25 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
				err := ws.Ping(pctx)
				cancel()
				if err != nil {
					ws.CloseNow()
					return
				}
			}
		}
	}()

	caller := Caller{DeviceID: ep.DeviceID}
	for {
		typ, msg, err := ws.Read(ctx)
		if err != nil {
			return err
		}
		if typ != websocket.MessageText {
			continue
		}
		go func() {
			if resp := s.Handle(ctx, caller, msg); resp != nil {
				wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
				defer cancel()
				ws.Write(wctx, websocket.MessageText, resp)
			}
		}()
	}
}

// redact hides access tokens in logged URLs.
func redact(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<invalid url>"
	}
	q := u.Query()
	for k := range q {
		if strings.Contains(strings.ToLower(k), "token") || strings.Contains(strings.ToLower(k), "key") {
			q.Set(k, "***")
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}
