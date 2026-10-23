package ai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/plant-squad/ptt-walkie/internal/api"
	"github.com/plant-squad/ptt-walkie/internal/archive"
	"github.com/plant-squad/ptt-walkie/internal/store"
	"github.com/plant-squad/ptt-walkie/internal/voice"
)

type rig struct {
	st  *store.Store
	arc *archive.Archive
	svc *Service
	ts  *httptest.Server
}

func newRig(t *testing.T, client *Client) *rig {
	t.Helper()
	st, _ := store.New("")
	apiSrv := &api.Server{Store: st}
	hub := voice.NewHub(st, apiSrv, time.Minute)
	apiSrv.Presence = hub
	arc, _ := archive.Open(t.TempDir())
	hub.AddRecorder(arc)
	h := &archive.Handler{Archive: arc, Store: st, Hub: hub}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ws", hub.ServeWS)
	h.Register(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	svc := &Service{Client: client, Archive: arc, Replay: h, Store: st, Hub: hub,
		PublicURL: ts.URL, AlertWords: []string{"事故", "封路"}, CatchUpAfter: 100 * time.Millisecond}
	svc.Attach()
	svc.Register(mux)
	return &rig{st, arc, svc, ts}
}

type dev struct {
	t  *testing.T
	ws *websocket.Conn
	in chan any
}

func (r *rig) dial(t *testing.T, id string) *dev {
	ws, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(r.ts.URL, "http")+"/ws?device_id="+id+"&name="+id, nil)
	if err != nil {
		t.Fatal(err)
	}
	d := &dev{t, ws, make(chan any, 256)}
	go func() {
		for {
			typ, data, err := ws.Read(context.Background())
			if err != nil {
				return
			}
			if typ == websocket.MessageBinary {
				d.in <- data
				continue
			}
			var m map[string]any
			json.Unmarshal(data, &m)
			d.in <- m
		}
	}()
	t.Cleanup(func() { ws.CloseNow() })
	d.wait("welcome")
	return d
}

func (d *dev) send(v any) {
	b, _ := json.Marshal(v)
	d.ws.Write(context.Background(), websocket.MessageText, b)
}

func (d *dev) wait(typ string) map[string]any {
	d.t.Helper()
	timeout := time.After(3 * time.Second)
	for {
		select {
		case v := <-d.in:
			if m, ok := v.(map[string]any); ok && m["type"] == typ {
				return m
			}
		case <-timeout:
			d.t.Fatalf("timeout waiting for %s", typ)
		}
	}
}

// talk transmits n 60ms SILK frames and returns the utterance id.
func (d *dev) talk(n int) string {
	d.send(map[string]string{"type": "ptt_start"})
	m := d.wait("floor")
	for i := range n {
		d.ws.Write(context.Background(), websocket.MessageBinary, []byte{0x58, byte(i)})
	}
	d.send(map[string]string{"type": "ptt_stop"})
	return m["utterance_id"].(string)
}

func fakeAI(t *testing.T, asrText, chatText string, asrURL *atomic.Value) *httptest.Server {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k" {
			http.Error(w, "unauthorized", 401)
			return
		}
		body, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/v1/voice/asr":
			var req struct {
				Model string
				Audio struct{ Format, URL string }
			}
			json.Unmarshal(body, &req)
			if req.Model != "asr" || req.Audio.Format != "ogg" {
				http.Error(w, "bad asr request", 400)
				return
			}
			asrURL.Store(req.Audio.URL)
			json.NewEncoder(w).Encode(map[string]any{"reqid": "x", "operation": "asr",
				"data": map[string]any{"audio_info": map[string]any{"duration": 300}, "result": map[string]any{"text": asrText}}})
		case "/v1/chat/completions":
			if !strings.Contains(string(body), asrText) {
				http.Error(w, "transcript missing from prompt", 400)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": chatText}}}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

func TestServerASRAlertAndSummary(t *testing.T) {
	var asrURL atomic.Value
	fake := fakeAI(t, "前方两公里事故，注意减速", "老王提醒前方两公里有事故，注意减速。", &asrURL)
	r := newRig(t, &Client{BaseURL: fake.URL + "/v1", Key: "k", Model: "m", ASRMode: "qiniu", ASRModel: "asr"})

	g, _ := r.st.CreateGroup("wang", store.CreateGroupInput{Name: "车队"})
	other, _ := r.st.CreateGroup("li", store.CreateGroupInput{Name: "老乡"})
	r.st.JoinGroup("li", g.ID)
	r.st.SwitchGroup("li", other.ID) // li listens elsewhere
	r.st.JoinGroup("zhang", g.ID)    // zhang tuned to 车队

	wang, li, zhang := r.dial(t, "wang"), r.dial(t, "li"), r.dial(t, "zhang")
	since := time.Now().Add(-time.Minute)
	uid := wang.talk(5)

	m := zhang.wait("transcript")
	if m["text"] != "前方两公里事故，注意减速" || m["utterance_id"] != uid {
		t.Fatalf("transcript %v", m)
	}
	if !strings.HasSuffix(asrURL.Load().(string), "/api/recordings/"+uid+"/audio.ogg") {
		t.Fatalf("asr url %v", asrURL.Load())
	}
	// ASR fetched our public URL: make sure it actually serves the audio.
	resp, err := http.Get(asrURL.Load().(string))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("audio url not served: %v", err)
	}

	n := li.wait("notice")
	if n["level"] != "alert" || !strings.Contains(n["text"].(string), "事故") {
		t.Fatalf("alert to member tuned elsewhere: %v", n)
	}
	rec, _ := r.arc.Get(uid)
	if len(rec.Alert) != 1 || rec.Alert[0] != "事故" {
		t.Fatalf("record alert %v", rec.Alert)
	}

	sum, err := r.svc.Summarize(context.Background(), "zhang", g.ID, since)
	if err != nil || !sum.ByLLM || sum.Text != "老王提醒前方两公里有事故，注意减速。" || sum.Count != 1 {
		t.Fatalf("summary %+v %v", sum, err)
	}
	// the speaker has missed nothing
	sum, _ = r.svc.Summarize(context.Background(), "wang", g.ID, since)
	if sum.Count != 0 || !strings.Contains(sum.Text, "没有新消息") {
		t.Fatalf("self summary %+v", sum)
	}
}

func TestClientTranscriptFallback(t *testing.T) {
	r := newRig(t, nil)
	g, _ := r.st.CreateGroup("wang", store.CreateGroupInput{Name: "车队"})
	r.st.JoinGroup("li", g.ID)
	wang, li := r.dial(t, "wang"), r.dial(t, "li")

	since := time.Now().Add(-time.Minute)
	uid := wang.talk(4)
	// transcript races the archive: it may arrive before the record exists
	wang.send(map[string]string{"type": "transcript", "utterance_id": uid, "text": "前面封路了走辅路"})
	if m := li.wait("transcript"); m["text"] != "前面封路了走辅路" {
		t.Fatalf("transcript %v", m)
	}
	// a listener cannot rewrite someone else's transcript
	li.send(map[string]string{"type": "transcript", "utterance_id": uid, "text": "伪造"})
	time.Sleep(100 * time.Millisecond)
	if rec, _ := r.arc.Get(uid); rec.Transcript != "前面封路了走辅路" {
		t.Fatalf("transcript overwritten: %q", rec.Transcript)
	}

	sum, _ := r.svc.Summarize(context.Background(), "li", g.ID, since)
	if sum.ByLLM || !strings.Contains(sum.Text, "wang说「前面封路了走辅路」") {
		t.Fatalf("fallback summary %q", sum.Text)
	}

}

func TestSpeakerCountSummaryAndHTTP(t *testing.T) {
	r := newRig(t, nil)
	g, _ := r.st.CreateGroup("wang", store.CreateGroupInput{Name: "车队"})
	r.st.JoinGroup("li", g.ID)
	wang := r.dial(t, "wang")
	wang.talk(3)
	wang.talk(3)
	deadline := time.Now().Add(2 * time.Second)
	for len(r.arc.Recent(g.ID, 5)) < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	resp, err := http.Get(r.ts.URL + "/api/groups/" + g.ID + "/summary?minutes=10&device_id=li")
	if err != nil {
		t.Fatal(err)
	}
	var sum Summary
	json.NewDecoder(resp.Body).Decode(&sum)
	if sum.Count != 2 || sum.Text != "过去10分钟车队有2条语音，来自wang2条。说「再放一遍」可以回放。" {
		t.Fatalf("summary %q (%d)", sum.Text, sum.Count)
	}
}

func TestOfflineCatchUp(t *testing.T) {
	r := newRig(t, nil)
	g, _ := r.st.CreateGroup("wang", store.CreateGroupInput{Name: "车队"})
	r.st.JoinGroup("li", g.ID)
	wang := r.dial(t, "wang")
	li := r.dial(t, "li")
	li.ws.Close(websocket.StatusNormalClosure, "tunnel")
	time.Sleep(200 * time.Millisecond)

	wang.talk(3)
	deadline := time.Now().Add(2 * time.Second)
	for len(r.arc.Recent(g.ID, 1)) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	li = r.dial(t, "li")
	n := li.wait("notice")
	if !strings.Contains(n["text"].(string), "离线期间车队有1条语音") {
		t.Fatalf("catch-up notice %v", n)
	}
	li.t = t
	timeout := time.After(6 * time.Second)
	for {
		select {
		case v := <-li.in:
			if m, ok := v.(map[string]any); ok && m["type"] == "talk_start" {
				if m["replay"] != true {
					t.Fatalf("expected replay %v", m)
				}
				return
			}
		case <-timeout:
			t.Fatal("no catch-up replay")
		}
	}
}

func TestMatchAlerts(t *testing.T) {
	if got := MatchAlerts("前面查车，还有封路", []string{"事故", "查车", "封路", ""}); len(got) != 2 {
		t.Fatalf("%v", got)
	}
}
