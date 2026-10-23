package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/plant-squad/ptt-walkie/internal/store"
)

func newServer(t *testing.T) *httptest.Server {
	t.Helper()
	st, _ := store.New("")
	mux := http.NewServeMux()
	(&Server{Store: st, BaseURL: "http://ptt.test", NearbyRadiusKm: 3}).Register(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func do(t *testing.T, ts *httptest.Server, method, path string, body any, out any) int {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}
	req, _ := http.NewRequest(method, ts.URL+path, &buf)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

func TestGroupLifecycle(t *testing.T) {
	ts := newServer(t)

	var g GroupStatus
	if code := do(t, ts, "POST", "/api/groups", map[string]any{"name": "京沪线车队", "owner_id": "lead"}, &g); code != 201 {
		t.Fatalf("create: %d", code)
	}
	if g.ShareURL != "http://ptt.test/join/"+g.Code {
		t.Fatalf("share url %q", g.ShareURL)
	}

	// join by spoken code
	var joined GroupStatus
	if code := do(t, ts, "POST", "/api/codes/"+g.Code+"/join", map[string]any{"device_id": "d1", "name": "老王"}, &joined); code != 200 {
		t.Fatalf("join: %d", code)
	}
	if len(joined.Members) != 2 || joined.Members[1].Name != "老王" {
		t.Fatalf("members %+v", joined.Members)
	}

	do(t, ts, "POST", "/api/groups", map[string]any{"name": "河南老乡群", "owner_id": "d1"}, nil)

	var sw GroupStatus
	if code := do(t, ts, "POST", "/api/devices/d1/switch", map[string]any{"query": "车队那个"}, &sw); code != 200 || sw.ID != g.ID {
		t.Fatalf("switch by name: %d %+v", code, sw)
	}
	if code := do(t, ts, "POST", "/api/devices/d1/switch", map[string]any{"delta": 1}, &sw); code != 200 || sw.Name != "河南老乡群" {
		t.Fatalf("switch next: %d %s", code, sw.Name)
	}

	var dev DeviceStatus
	do(t, ts, "GET", "/api/devices/d1", nil, &dev)
	if dev.Current == nil || dev.Current.Name != "河南老乡群" || len(dev.List) != 2 {
		t.Fatalf("device %+v", dev)
	}

	// non-leader cannot dissolve
	req, _ := http.NewRequest("DELETE", ts.URL+"/api/groups/"+g.ID, nil)
	req.Header.Set("X-Device-Id", "d1")
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 403 {
		t.Fatalf("dissolve by member: %d", resp.StatusCode)
	}

	resp, _ = http.Get(ts.URL + "/api/groups/" + g.ID + "/qr.png")
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/png" {
		t.Fatalf("qr: %d", resp.StatusCode)
	}
}

func TestNearbyUsesDeviceLocation(t *testing.T) {
	ts := newServer(t)
	do(t, ts, "POST", "/api/devices/a/location", map[string]any{"lat": 31.23, "lng": 121.47}, nil)
	var g GroupStatus
	if code := do(t, ts, "POST", "/api/groups", map[string]any{"name": "服务区", "owner_id": "a", "type": "temp", "visibility": "nearby"}, &g); code != 201 {
		t.Fatalf("create nearby: %d", code)
	}
	var list []GroupStatus
	do(t, ts, "GET", "/api/nearby?lat=31.231&lng=121.471", nil, &list)
	if len(list) != 1 || list[0].ID != g.ID {
		t.Fatalf("nearby %+v", list)
	}
	if code := do(t, ts, "GET", "/api/nearby", nil, nil); code != 400 {
		t.Fatalf("nearby without location: %d", code)
	}
}

func TestBatchJoin(t *testing.T) {
	ts := newServer(t)
	var g, other GroupStatus
	do(t, ts, "POST", "/api/groups", map[string]any{"name": "顺丰华东车队"}, &g)
	do(t, ts, "POST", "/api/groups", map[string]any{"name": "家人", "owner_id": "car-2"}, &other)

	var out struct{ Joined, Failed []string }
	do(t, ts, "POST", "/api/groups/"+g.ID+"/members/batch", map[string]any{
		"devices": []map[string]string{{"device_id": "car-1", "name": "沪A12345"}, {"device_id": "car-2"}, {"device_id": " "}},
	}, &out)
	if len(out.Joined) != 2 || len(out.Failed) != 1 {
		t.Fatalf("batch %+v", out)
	}
	var d DeviceStatus
	do(t, ts, "GET", "/api/devices/car-2", nil, &d)
	if d.CurrentGroup != other.ID {
		t.Fatal("unpinned import must not steal the current channel")
	}
	do(t, ts, "POST", "/api/groups/"+g.ID+"/members/batch", map[string]any{
		"devices": []map[string]string{{"device_id": "car-2"}}, "pin": true}, nil)
	do(t, ts, "GET", "/api/devices/car-2", nil, &d)
	if d.CurrentGroup != g.ID {
		t.Fatal("pinned import should switch channel")
	}
	do(t, ts, "GET", "/api/devices/car-1", nil, &d)
	if d.Name != "沪A12345" {
		t.Fatalf("name %q", d.Name)
	}
}

func TestAdminTokenRequiredForAnonymousAdminActions(t *testing.T) {
	st, _ := store.New("")
	mux := http.NewServeMux()
	(&Server{Store: st, AdminToken: "s3cret"}).Register(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()
	g, _ := st.CreateGroup("lead", store.CreateGroupInput{Name: "车队"})
	st.JoinGroup("me", g.ID)

	if code := do(t, ts, "PUT", "/api/groups/"+g.ID+"/members/me/role", map[string]any{"role": "leader"}, nil); code != 403 {
		t.Fatalf("anonymous promote: %d", code)
	}
	if code := do(t, ts, "DELETE", "/api/groups/"+g.ID, nil, nil); code != 403 {
		t.Fatalf("anonymous dissolve: %d", code)
	}
	if code := do(t, ts, "POST", "/api/groups/"+g.ID+"/members/batch", map[string]any{"devices": []any{}}, nil); code != 403 {
		t.Fatalf("anonymous batch: %d", code)
	}
	// leaders act with their identity; the console with the token
	if code := do(t, ts, "PUT", "/api/groups/"+g.ID+"/members/me/role", map[string]any{"role": "leader", "actor_id": "lead"}, nil); code != 204 {
		t.Fatalf("leader promote: %d", code)
	}
	if code := do(t, ts, "DELETE", "/api/groups/"+g.ID+"?token=s3cret", nil, nil); code != 204 {
		t.Fatalf("token dissolve: %d", code)
	}
}
