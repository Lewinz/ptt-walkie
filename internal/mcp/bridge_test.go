package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestParseEndpoints(t *testing.T) {
	eps := ParseEndpoints("wss://a/mcp?token=x|dev-1; wss://b/mcp?device_id=dev-2&token=y\nwss://c/mcp")
	if len(eps) != 3 || eps[0].URL != "wss://a/mcp?token=x" || eps[0].DeviceID != "dev-1" ||
		eps[1].DeviceID != "dev-2" || eps[2].DeviceID != "" {
		t.Fatalf("%+v", eps)
	}
	if got := redact("wss://a/mcp?token=secret&x=1"); strings.Contains(got, "secret") {
		t.Fatalf("not redacted: %s", got)
	}
}

// The fake platform dials nothing; it accepts our bridge and sends requests.
func TestBridgeServesRequests(t *testing.T) {
	srv := NewServer(Deps{})
	srv.AddTool(&Tool{Name: "whoami", Description: "test", InputSchema: schema(props{}),
		handler: func(_ context.Context, c Caller, _ Args) Result { return Result{Text: "device=" + c.DeviceID} }})

	got := make(chan map[string]any, 4)
	platform := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		ctx := r.Context()
		ws.Write(ctx, websocket.MessageText, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}`))
		ws.Write(ctx, websocket.MessageText, []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
		ws.Write(ctx, websocket.MessageText, []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"whoami"}}`))
		for range 2 {
			_, data, err := ws.Read(ctx)
			if err != nil {
				return
			}
			var m map[string]any
			json.Unmarshal(data, &m)
			got <- m
		}
		<-ctx.Done()
	}))
	defer platform.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.RunBridge(ctx, Endpoint{URL: "ws" + strings.TrimPrefix(platform.URL, "http"), DeviceID: "car-7"})

	byID := map[float64]map[string]any{}
	for range 2 {
		select {
		case m := <-got:
			byID[m["id"].(float64)] = m
		case <-time.After(3 * time.Second):
			t.Fatal("timeout")
		}
	}
	if byID[1]["result"].(map[string]any)["protocolVersion"] != "2024-11-05" {
		t.Fatalf("init %v", byID[1])
	}
	text := byID[2]["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"]
	if text != "device=car-7" {
		t.Fatalf("whoami %v", text)
	}
}
