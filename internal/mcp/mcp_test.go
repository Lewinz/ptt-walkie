package mcp

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/plant-squad/ptt-walkie/internal/api"
	"github.com/plant-squad/ptt-walkie/internal/archive"
	"github.com/plant-squad/ptt-walkie/internal/store"
	"github.com/plant-squad/ptt-walkie/internal/voice"
)

type fixture struct {
	st *store.Store
	ts *httptest.Server
}

func newFixture(t *testing.T, token string) *fixture {
	t.Helper()
	st, _ := store.New("")
	apiSrv := &api.Server{Store: st, BaseURL: "http://ptt.test", NearbyRadiusKm: 3}
	hub := voice.NewHub(st, apiSrv, time.Minute)
	apiSrv.Presence = hub
	arc, _ := archive.Open(t.TempDir())
	srv := NewServer(Deps{Store: st, API: apiSrv, Hub: hub,
		Archive: &archive.Handler{Archive: arc, Store: st, Hub: hub}, NearbyRadiusKm: 3})
	mux := http.NewServeMux()
	(&HTTPHandler{Server: srv, Token: token}).Register(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return &fixture{st, ts}
}

var rpcID int

func (f *fixture) rpc(t *testing.T, path, method string, params any, hdr ...string) (int, map[string]any) {
	t.Helper()
	rpcID++
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": rpcID, "method": method, "params": params})
	req, _ := http.NewRequest("POST", f.ts.URL+path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var out map[string]any
	json.Unmarshal(data, &out)
	return resp.StatusCode, out
}

// call invokes a tool as device and returns its text and isError flag.
func (f *fixture) call(t *testing.T, device, tool string, args map[string]any) (string, bool) {
	t.Helper()
	path := "/mcp"
	if device != "" {
		path += "/d/" + device
	}
	_, out := f.rpc(t, path, "tools/call", map[string]any{"name": tool, "arguments": args})
	res, ok := out["result"].(map[string]any)
	if !ok {
		t.Fatalf("%s: no result: %v", tool, out)
	}
	text := res["content"].([]any)[0].(map[string]any)["text"].(string)
	return text, res["isError"] == true
}

func TestHandshake(t *testing.T) {
	f := newFixture(t, "")
	code, out := f.rpc(t, "/mcp", "initialize", map[string]any{
		"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "test", "version": "1"}})
	if code != 200 {
		t.Fatalf("initialize %d", code)
	}
	res := out["result"].(map[string]any)
	if res["protocolVersion"] != "2025-06-18" || res["instructions"] == "" {
		t.Fatalf("initialize result %v", res)
	}

	// notification → 202, no body
	resp, _ := http.Post(f.ts.URL+"/mcp", "application/json", strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("notification %d", resp.StatusCode)
	}

	_, out = f.rpc(t, "/mcp", "tools/list", nil)
	tools := out["result"].(map[string]any)["tools"].([]any)
	if len(tools) < 10 {
		t.Fatalf("tools: %d", len(tools))
	}
	for _, tl := range tools {
		m := tl.(map[string]any)
		if m["inputSchema"].(map[string]any)["type"] != "object" || m["description"] == "" {
			t.Fatalf("bad tool %v", m)
		}
	}

	_, out = f.rpc(t, "/mcp", "nope", nil)
	if out["error"].(map[string]any)["code"].(float64) != codeMethodNotFound {
		t.Fatalf("unknown method %v", out)
	}

	// unversioned client falls back to latest
	_, out = f.rpc(t, "/mcp", "initialize", map[string]any{"protocolVersion": "1999-01-01"})
	if out["result"].(map[string]any)["protocolVersion"] != protocolVersions[0] {
		t.Fatal("version fallback")
	}
}

func TestAuth(t *testing.T) {
	f := newFixture(t, "s3cret")
	if code, _ := f.rpc(t, "/mcp", "ping", nil); code != 401 {
		t.Fatalf("no token: %d", code)
	}
	if code, _ := f.rpc(t, "/mcp", "ping", nil, "Authorization", "Bearer s3cret"); code != 200 {
		t.Fatalf("bearer: %d", code)
	}
	if code, _ := f.rpc(t, "/mcp?token=s3cret", "ping", nil); code != 200 {
		t.Fatalf("query token: %d", code)
	}
}

func TestVoiceCommandFlow(t *testing.T) {
	f := newFixture(t, "")

	// identity is required
	if text, isErr := f.call(t, "", "list_my_groups", nil); !isErr || !strings.Contains(text, "设备") {
		t.Fatalf("no device: %q", text)
	}

	text, isErr := f.call(t, "lead", "create_group", map[string]any{"name": "京沪线车队"})
	if isErr || !strings.Contains(text, "京沪线车队") {
		t.Fatalf("create: %s", text)
	}
	groups := f.st.AllGroups()
	code := groups[0].Code
	spoken := SpokenCode(code)
	if !strings.Contains(text, spoken) {
		t.Fatalf("create text should contain spoken code %s: %s", spoken, text)
	}

	// join by spoken Chinese digits
	text, isErr = f.call(t, "wang", "join_group", map[string]any{"query": "房间" + spoken})
	if isErr || !strings.Contains(text, "已加入京沪线车队") {
		t.Fatalf("join: %s", text)
	}
	f.call(t, "wang", "create_group", map[string]any{"name": "河南老乡群"})

	text, _ = f.call(t, "wang", "switch_group", map[string]any{"query": "车队那个"})
	if !strings.HasPrefix(text, "已切换到京沪线车队") {
		t.Fatalf("switch: %s", text)
	}
	text, _ = f.call(t, "wang", "switch_group", map[string]any{"direction": "next"})
	if !strings.HasPrefix(text, "已切换到河南老乡群") {
		t.Fatalf("next: %s", text)
	}
	text, _ = f.call(t, "wang", "list_my_groups", nil)
	if !strings.Contains(text, "2个群") || !strings.Contains(text, "河南老乡群，0人在线（当前）") {
		t.Fatalf("list: %s", text)
	}
	text, _ = f.call(t, "wang", "get_group_status", map[string]any{"query": "车队"})
	if !strings.Contains(text, "共2人") {
		t.Fatalf("status: %s", text)
	}
	text, _ = f.call(t, "wang", "mute_group", map[string]any{"minutes": "30"})
	if text != "河南老乡群已静音30分钟。" {
		t.Fatalf("mute: %s", text)
	}
	text, _ = f.call(t, "wang", "share_group", nil)
	if !strings.Contains(text, "http://ptt.test/join/") {
		t.Fatalf("share: %s", text)
	}
	if text, isErr = f.call(t, "wang", "dissolve_group", map[string]any{"query": "车队"}); !isErr {
		t.Fatalf("member dissolved: %s", text)
	}
	if text, isErr = f.call(t, "wang", "replay_last", nil); !isErr || !strings.Contains(text, "不在线") {
		t.Fatalf("replay offline: %s", text)
	}
	text, _ = f.call(t, "wang", "who_spoke_last", nil)
	if !strings.Contains(text, "最近没人说话") {
		t.Fatalf("who: %s", text)
	}
	text, _ = f.call(t, "wang", "leave_group", nil)
	if text != "已退出河南老乡群。现在在京沪线车队。" {
		t.Fatalf("leave: %s", text)
	}
	if _, isErr = f.call(t, "lead", "dissolve_group", nil); isErr {
		t.Fatal("leader dissolve failed")
	}
}

func TestNearbyAndRouteTools(t *testing.T) {
	f := newFixture(t, "")
	if text, isErr := f.call(t, "a", "list_nearby_groups", nil); !isErr || !strings.Contains(text, "位置") {
		t.Fatalf("no location: %s", text)
	}
	f.st.UpdateLocation("a", store.GeoPoint{Lat: 30.5, Lng: 114.3})
	f.st.UpdateLocation("b", store.GeoPoint{Lat: 30.501, Lng: 114.301})
	text, isErr := f.call(t, "a", "create_group", map[string]any{"name": "服务区拼车", "visibility": "nearby"})
	if isErr || !strings.Contains(text, "临时群") {
		t.Fatalf("create nearby: %s", text)
	}
	text, _ = f.call(t, "b", "list_nearby_groups", nil)
	if !strings.Contains(text, "服务区拼车") {
		t.Fatalf("nearby: %s", text)
	}
	text, isErr = f.call(t, "b", "join_group", map[string]any{"query": "服务区拼车群"})
	if isErr || !strings.Contains(text, "已加入服务区拼车") {
		t.Fatalf("join nearby by name: %s", text)
	}

	f.call(t, "c", "create_group", map[string]any{"name": "沪蓉冷链车队", "visibility": "route", "route": "上海-成都 G42"})
	text, _ = f.call(t, "d", "list_route_groups", map[string]any{"route": "G42"})
	if !strings.Contains(text, "沪蓉冷链车队") {
		t.Fatalf("route: %s", text)
	}
}

func TestArgsLenient(t *testing.T) {
	a := Args{"n": "3", "f": 2.5, "b": "是", "s": 3825.0}
	if a.Int("n", 0) != 3 || a.Float("f", 0) != 2.5 || !a.Bool("b", false) || a.String("s") != "3825" {
		t.Fatal("lenient args")
	}
}
