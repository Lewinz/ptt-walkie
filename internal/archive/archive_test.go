package archive

import (
	"bytes"
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
	"github.com/plant-squad/ptt-walkie/internal/voice"
)

// 0x58: SILK wideband, 60 ms, one frame.
func pkt(b byte) []byte { return []byte{0x58, b, b} }

func TestPacketSamples(t *testing.T) {
	cases := []struct {
		pkt  []byte
		want int
	}{
		{[]byte{0x58}, 2880},       // SILK 60ms
		{[]byte{0x08}, 960},        // SILK NB 20ms
		{[]byte{0xF8}, 960},        // CELT FB 20ms
		{[]byte{0xF9}, 1920},       // CELT 20ms x2
		{[]byte{0xFB, 0x03}, 2880}, // CELT 20ms x3 (code 3)
		{nil, 0},
	}
	for _, c := range cases {
		if got := PacketSamples(c.pkt); got != c.want {
			t.Errorf("PacketSamples(%x) = %d, want %d", c.pkt, got, c.want)
		}
	}
}

func TestOggRoundTrip(t *testing.T) {
	in := [][]byte{pkt(1), bytes.Repeat([]byte{0x58, 7}, 300), pkt(3)} // middle one spans >255 lacing
	var buf bytes.Buffer
	if err := EncodeOggOpus(&buf, 16000, 1, in); err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(buf.Bytes(), []byte("OggS")) {
		t.Fatal("not ogg")
	}
	out, err := DecodeOggOpus(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != len(in) {
		t.Fatalf("got %d packets", len(out))
	}
	for i := range in {
		if !bytes.Equal(in[i], out[i]) {
			t.Fatalf("packet %d differs", i)
		}
	}
}

func TestArchivePersistAndQuery(t *testing.T) {
	dir := t.TempDir()
	a, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	saved := make(chan Record, 4)
	a.OnSaved(func(r Record) { saved <- r })

	start := time.Now()
	u := voice.Utterance{ID: "u1", GroupID: "g1", SpeakerID: "a", SpeakerName: "老王",
		StartedAt: start, Audio: voice.DefaultAudio}
	a.Start(u)
	for i := range 5 {
		a.Frame("u1", pkt(byte(i)))
	}
	a.End(u)

	// accidental press: too short, not archived
	short := voice.Utterance{ID: "u2", GroupID: "g1", StartedAt: start}
	a.Start(short)
	a.Frame("u2", pkt(1))
	a.End(short)

	rec := <-saved
	if rec.DurationMs != 300 {
		t.Fatalf("duration %d", rec.DurationMs)
	}
	if got := a.Recent("g1", 10); len(got) != 1 {
		t.Fatalf("recent %d", len(got))
	}
	a.Update("u1", func(r *Record) { r.Transcript = "前面堵车" })
	a.Close()

	b, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := b.Get("u1")
	if !ok || got.Transcript != "前面堵车" || got.SpeakerName != "老王" {
		t.Fatalf("reloaded %+v", got)
	}
	pkts, err := b.Packets(got)
	if err != nil || len(pkts) != 5 || pkts[4][1] != 4 {
		t.Fatalf("packets %v %v", pkts, err)
	}
	if len(b.Since("g1", start.Add(-time.Second))) != 1 || len(b.Since("g1", start.Add(time.Second))) != 0 {
		t.Fatal("since filter")
	}
}

func TestReplayToDevice(t *testing.T) {
	st, _ := store.New("")
	apiSrv := &api.Server{Store: st}
	hub := voice.NewHub(st, apiSrv, time.Minute)
	apiSrv.Presence = hub
	a, _ := Open(t.TempDir())
	hub.AddRecorder(a)
	h := &Handler{Archive: a, Store: st, Hub: hub}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ws", hub.ServeWS)
	h.Register(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	g, _ := st.CreateGroup("talker", store.CreateGroupInput{Name: "车队"})
	st.JoinGroup("me", g.ID)

	dial := func(id string) (*websocket.Conn, chan any) {
		ws, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(ts.URL, "http")+"/ws?device_id="+id, nil)
		if err != nil {
			t.Fatal(err)
		}
		ch := make(chan any, 100)
		go func() {
			for {
				typ, data, err := ws.Read(context.Background())
				if err != nil {
					return
				}
				if typ == websocket.MessageBinary {
					ch <- data
					continue
				}
				var m map[string]any
				json.Unmarshal(data, &m)
				ch <- m
			}
		}()
		t.Cleanup(func() { ws.CloseNow() })
		return ws, ch
	}
	talker, tch := dial("talker")
	_, mch := dial("me")
	waitType := func(ch chan any, typ string) map[string]any {
		t.Helper()
		for {
			select {
			case v := <-ch:
				if m, ok := v.(map[string]any); ok && m["type"] == typ {
					return m
				}
			case <-time.After(3 * time.Second):
				t.Fatalf("timeout waiting %s", typ)
			}
		}
	}

	send := func(v any) {
		b, _ := json.Marshal(v)
		talker.Write(context.Background(), websocket.MessageText, b)
	}
	send(map[string]string{"type": "ptt_start"})
	waitType(tch, "floor")
	for i := range 4 {
		talker.Write(context.Background(), websocket.MessageBinary, pkt(byte(i)))
	}
	send(map[string]string{"type": "ptt_stop"})
	waitType(mch, "talk_stop")

	// Wait for the record to be indexed, then ask for a replay.
	deadline := time.Now().Add(2 * time.Second)
	for len(a.Recent(g.ID, 1)) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	resp, err := http.Post(ts.URL+"/api/devices/me/replay", "application/json", strings.NewReader(`{"n":1}`))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("replay: %v %v", err, resp.Status)
	}
	m := waitType(mch, "talk_start")
	if m["replay"] != true || m["speaker_id"] != "talker" {
		t.Fatalf("replay talk_start %v", m)
	}
	n := 0
	for n < 4 {
		select {
		case v := <-mch:
			if _, ok := v.([]byte); ok {
				n++
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("got %d replay frames", n)
		}
	}
	if m := waitType(mch, "talk_stop"); m["reason"] != "replay_end" {
		t.Fatalf("stop %v", m)
	}

	// The speaker has nothing (of others) to replay.
	resp, _ = http.Post(ts.URL+"/api/devices/talker/replay", "application/json", nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("self replay: %d", resp.StatusCode)
	}

	resp, _ = http.Get(ts.URL + "/api/recordings/" + a.Recent(g.ID, 1)[0].ID + "/audio.ogg")
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "audio/ogg" {
		t.Fatalf("audio download: %d", resp.StatusCode)
	}
}
