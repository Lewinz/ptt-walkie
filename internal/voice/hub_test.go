package voice

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/plant-squad/ptt-walkie/internal/api"
	"github.com/plant-squad/ptt-walkie/internal/store"
)

type env struct {
	st  *store.Store
	hub *Hub
	ts  *httptest.Server
}

func newEnv(t *testing.T) *env {
	t.Helper()
	st, _ := store.New("")
	apiSrv := &api.Server{Store: st, BaseURL: "http://x"}
	hub := NewHub(st, apiSrv, 2*time.Second)
	apiSrv.Presence = hub
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ws", hub.ServeWS)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return &env{st, hub, ts}
}

type frame struct {
	text bool
	msg  map[string]any
	bin  []byte
}

type client struct {
	t  *testing.T
	ws *websocket.Conn
	in chan frame
}

func (e *env) dial(t *testing.T, id string) *client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	url := "ws" + strings.TrimPrefix(e.ts.URL, "http") + "/ws?device_id=" + id + "&name=" + id
	ws, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	c := &client{t: t, ws: ws, in: make(chan frame, 256)}
	go func() {
		for {
			typ, data, err := ws.Read(context.Background())
			if err != nil {
				close(c.in)
				return
			}
			if typ == websocket.MessageBinary {
				c.in <- frame{bin: data}
				continue
			}
			var m map[string]any
			json.Unmarshal(data, &m)
			c.in <- frame{text: true, msg: m}
		}
	}()
	t.Cleanup(func() { ws.CloseNow() })
	c.expect("welcome")
	return c
}

func (c *client) send(v any) {
	data, _ := json.Marshal(v)
	if err := c.ws.Write(context.Background(), websocket.MessageText, data); err != nil {
		c.t.Fatal(err)
	}
}

func (c *client) audio(b []byte) {
	if err := c.ws.Write(context.Background(), websocket.MessageBinary, b); err != nil {
		c.t.Fatal(err)
	}
}

// expect skips presence chatter until a message of the given type arrives.
func (c *client) expect(typ string) map[string]any {
	c.t.Helper()
	timeout := time.After(2 * time.Second)
	for {
		select {
		case f, ok := <-c.in:
			if !ok {
				c.t.Fatalf("closed while waiting for %s", typ)
			}
			if f.text && f.msg["type"] == typ {
				return f.msg
			}
			if f.text && f.msg["type"] != "presence" && f.msg["type"] != "channel" {
				c.t.Logf("skipping %v while waiting for %s", f.msg, typ)
			}
		case <-timeout:
			c.t.Fatalf("timeout waiting for %s", typ)
		}
	}
}

func (c *client) expectAudio() []byte {
	c.t.Helper()
	timeout := time.After(2 * time.Second)
	for {
		select {
		case f := <-c.in:
			if !f.text {
				return f.bin
			}
		case <-timeout:
			c.t.Fatal("timeout waiting for audio")
		}
	}
}

func (c *client) expectSilence(d time.Duration) {
	c.t.Helper()
	timeout := time.After(d)
	for {
		select {
		case f := <-c.in:
			if !f.text || f.msg["type"] == "talk_start" {
				c.t.Fatalf("unexpected %v", f)
			}
		case <-timeout:
			return
		}
	}
}

func TestTalkAndFloorContention(t *testing.T) {
	e := newEnv(t)
	g, _ := e.st.CreateGroup("lead", store.CreateGroupInput{Name: "车队"})
	e.st.JoinGroup("a", g.ID)
	e.st.JoinGroup("b", g.ID)

	a, b, lead := e.dial(t, "a"), e.dial(t, "b"), e.dial(t, "lead")

	a.send(map[string]string{"type": "ptt_start"})
	if m := a.expect("floor"); m["granted"] != true {
		t.Fatalf("a floor %v", m)
	}
	a.audio([]byte{1, 2, 3})
	if m := b.expect("talk_start"); m["speaker_id"] != "a" {
		t.Fatalf("talk_start %v", m)
	}
	if got := b.expectAudio(); string(got) != "\x01\x02\x03" {
		t.Fatalf("audio %v", got)
	}
	if e.hub.Speaker(g.ID) != "a" {
		t.Fatal("speaker not reported")
	}

	// b can't grab the floor while a talks
	b.send(map[string]string{"type": "ptt_start"})
	if m := b.expect("floor"); m["granted"] != false || m["reason"] != "busy" || m["holder_id"] != "a" {
		t.Fatalf("b floor %v", m)
	}

	// leader preempts
	lead.expect("talk_start")
	lead.send(map[string]string{"type": "ptt_start"})
	if m := lead.expect("floor"); m["granted"] != true {
		t.Fatalf("leader floor %v", m)
	}
	if m := a.expect("floor_revoked"); m["reason"] != "preempted" {
		t.Fatalf("revoked %v", m)
	}
	if m := b.expect("talk_stop"); m["reason"] != "preempted" {
		t.Fatalf("talk_stop %v", m)
	}
	lead.audio([]byte{9})
	if m := b.expect("talk_start"); m["speaker_id"] != "lead" || m["leader"] != true {
		t.Fatalf("leader talk_start %v", m)
	}
	lead.send(map[string]string{"type": "ptt_stop"})
	b.expect("talk_stop")
	if e.hub.Speaker(g.ID) != "" {
		t.Fatal("floor not released")
	}
}

func TestMuteLeaderOnlyAndSwitchPush(t *testing.T) {
	e := newEnv(t)
	g, _ := e.st.CreateGroup("lead", store.CreateGroupInput{Name: "车队"})
	other, _ := e.st.CreateGroup("x", store.CreateGroupInput{Name: "河南老乡群"})
	e.st.JoinGroup("a", g.ID)
	e.st.JoinGroup("b", g.ID)
	e.st.JoinGroup("b", other.ID)
	e.st.SwitchGroup("b", g.ID)

	a, b := e.dial(t, "a"), e.dial(t, "b")
	e.st.SetLeaderOnly("b", g.ID, true)
	time.Sleep(50 * time.Millisecond)

	a.send(map[string]string{"type": "ptt_start"})
	a.expect("floor")
	a.audio([]byte{1})
	b.expectSilence(200 * time.Millisecond)
	a.send(map[string]string{"type": "ptt_stop"})

	// switching via the store (as MCP would) pushes a channel message
	e.st.SwitchGroup("b", other.ID)
	m := b.expect("channel")
	grp := m["group"].(map[string]any)
	if grp["name"] != "河南老乡群" || !strings.Contains(m["announce"].(string), "已切换到河南老乡群") {
		t.Fatalf("channel %v", m)
	}

	// switching by spoken name over the socket
	b.send(map[string]any{"type": "switch", "query": "车队那个"})
	m = b.expect("channel")
	if m["group"].(map[string]any)["id"] != g.ID {
		t.Fatalf("switch back %v", m)
	}
}

func TestEmergencyReachesMembersTunedElsewhere(t *testing.T) {
	e := newEnv(t)
	sos, _ := e.st.CreateGroup("dispatch", store.CreateGroupInput{Name: "紧急频道", Type: store.GroupEmergency})
	chat, _ := e.st.CreateGroup("a", store.CreateGroupInput{Name: "闲聊"})
	e.st.JoinGroup("b", sos.ID)
	e.st.JoinGroup("b", chat.ID) // b tuned to 闲聊

	a, b, d := e.dial(t, "a"), e.dial(t, "b"), e.dial(t, "dispatch")

	a.send(map[string]string{"type": "ptt_start"})
	a.expect("floor")
	a.audio([]byte{1})
	b.expect("talk_start")
	b.expectAudio()

	d.send(map[string]string{"type": "ptt_start"})
	d.expect("floor")
	d.audio([]byte{7})
	if m := b.expect("talk_stop"); m["reason"] != "preempted" {
		t.Fatalf("expected chat preempted, got %v", m)
	}
	if m := b.expect("talk_start"); m["group_id"] != sos.ID {
		t.Fatalf("expected emergency stream, got %v", m)
	}
	if got := b.expectAudio(); got[0] != 7 {
		t.Fatalf("audio %v", got)
	}
	// chat audio is dropped while emergency plays
	a.audio([]byte{2})
	d.audio([]byte{8})
	if got := b.expectAudio(); got[0] != 8 {
		t.Fatalf("expected emergency frame, got %v", got)
	}
}

func TestIdleFloorReleased(t *testing.T) {
	e := newEnv(t)
	g, _ := e.st.CreateGroup("a", store.CreateGroupInput{Name: "车队"})
	e.st.JoinGroup("b", g.ID)
	a, _ := e.dial(t, "a"), e.dial(t, "b")
	e.hub.IdleRelease = 100 * time.Millisecond

	a.send(map[string]string{"type": "ptt_start"})
	a.expect("floor")
	time.Sleep(200 * time.Millisecond)
	e.hub.sweep()
	if m := a.expect("floor_revoked"); m["reason"] != "idle" {
		t.Fatalf("revoked %v", m)
	}
	if e.hub.Speaker(g.ID) != "" {
		t.Fatal("floor still held")
	}
}

func TestDisconnectReleasesFloor(t *testing.T) {
	e := newEnv(t)
	g, _ := e.st.CreateGroup("a", store.CreateGroupInput{Name: "车队"})
	a := e.dial(t, "a")
	a.send(map[string]string{"type": "ptt_start"})
	a.expect("floor")
	a.ws.Close(websocket.StatusNormalClosure, "")
	deadline := time.Now().Add(2 * time.Second)
	for e.hub.Speaker(g.ID) != "" || e.hub.Online("a") {
		if time.Now().After(deadline) {
			t.Fatal("floor / presence not cleared on disconnect")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
