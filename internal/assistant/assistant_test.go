package assistant

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/plant-squad/ptt-walkie/internal/ai"
	"github.com/plant-squad/ptt-walkie/internal/api"
	"github.com/plant-squad/ptt-walkie/internal/archive"
	"github.com/plant-squad/ptt-walkie/internal/mcp"
	"github.com/plant-squad/ptt-walkie/internal/store"
	"github.com/plant-squad/ptt-walkie/internal/voice"
)

func TestParseIntent(t *testing.T) {
	cases := []struct {
		text string
		tool string
		args map[string]any
	}{
		{"建个群叫京沪线车队", "create_group", map[string]any{"name": "京沪线车队"}},
		{"小智，帮我建一个周末骑行群吧", "create_group", map[string]any{"name": "周末骑行群", "template": "cycling"}},
		{"建一个附近的群", "create_group", map[string]any{"name": "附近临时群", "visibility": "nearby", "type": "temp"}},
		{"建个跑沪蓉线的群", "create_group", map[string]any{"name": "沪蓉车队", "visibility": "route", "route": "沪蓉"}},
		{"加入房间三八二五", "join_group", map[string]any{"query": "三八二五"}},
		{"加入3825。", "join_group", map[string]any{"query": "3825"}},
		{"切到老乡群", "switch_group", map[string]any{"query": "老乡群"}},
		{"去车队那个", "switch_group", map[string]any{"query": "车队那个"}},
		{"下一个频道", "switch_group", map[string]any{"direction": "next"}},
		{"上一个", "switch_group", map[string]any{"direction": "previous"}},
		{"我在哪个群", "get_group_status", map[string]any{}},
		{"群里有谁在线", "get_group_status", map[string]any{}},
		{"现在谁在说话", "get_group_status", map[string]any{}},
		{"刚才是谁说的", "who_spoke_last", map[string]any{"n": 3}},
		{"我有哪些群", "list_my_groups", map[string]any{}},
		{"附近有什么群", "list_nearby_groups", map[string]any{}},
		{"有没有跑京沪线的群", "list_route_groups", map[string]any{"route": "京沪"}},
		{"刚才说的再放一遍", "replay_last", map[string]any{"n": 1}},
		{"重放最后三条", "replay_last", map[string]any{"n": 3}},
		{"我错过了什么", "summarize_missed", map[string]any{"minutes": 30}},
		{"最近一小时群里说了啥", "summarize_missed", map[string]any{"minutes": 60}},
		{"这个群先静音", "mute_group", map[string]any{"minutes": 60}},
		{"静音半小时", "mute_group", map[string]any{"minutes": 30}},
		{"静音两个小时", "mute_group", map[string]any{"minutes": 120}},
		{"取消静音", "mute_group", map[string]any{"minutes": 0}},
		{"只听车队长的", "listen_leader_only", map[string]any{"on": true}},
		{"恢复听所有人", "listen_leader_only", map[string]any{"on": false}},
		{"给车队群发：前面堵车", "send_text_message", map[string]any{"query": "车队群", "text": "前面堵车"}},
		{"告诉大家我晚点到", "send_text_message", map[string]any{"text": "我晚点到"}},
		{"房间号是多少", "share_group", map[string]any{}},
		{"退出这个群", "leave_group", map[string]any{}},
		{"退出老乡群", "leave_group", map[string]any{"query": "老乡群"}},
		{"解散这个群", "dissolve_group", map[string]any{}},
	}
	for _, c := range cases {
		got, ok := ParseIntent(c.text)
		if !ok || got.Tool != c.tool || !reflect.DeepEqual(got.Args, c.args) {
			t.Errorf("ParseIntent(%q) = %+v %v, want %s %v", c.text, got, ok, c.tool, c.args)
		}
	}
	if _, ok := ParseIntent("今天天气怎么样"); ok {
		t.Error("chit-chat should not match")
	}
}

func TestCNNumber(t *testing.T) {
	for in, want := range map[string]int{"十": 10, "十五": 15, "二十": 20, "三十五": 35, "一百二十": 120, "两": 2, "45": 45} {
		if got, ok := cnNumber(in); !ok || got != want {
			t.Errorf("cnNumber(%q) = %d", in, got)
		}
	}
}

func newMCP(t *testing.T) (*mcp.Server, *store.Store) {
	st, _ := store.New("")
	apiSrv := &api.Server{Store: st, BaseURL: "http://x"}
	hub := voice.NewHub(st, apiSrv, time.Minute)
	apiSrv.Presence = hub
	arc, _ := archive.Open(t.TempDir())
	h := &archive.Handler{Archive: arc, Store: st, Hub: hub}
	svc := &ai.Service{Archive: arc, Replay: h, Store: st, Hub: hub}
	svc.Attach()
	return mcp.NewServer(mcp.Deps{Store: st, API: apiSrv, Hub: hub, Archive: h, AI: svc, NearbyRadiusKm: 3}), st
}

func TestRulesEndToEnd(t *testing.T) {
	srv, st := newMCP(t)
	a := &Assistant{MCP: srv}
	ctx := context.Background()

	r := a.Handle(ctx, "me", "建个群叫京沪线车队")
	if !strings.HasPrefix(r.Text, "已创建群「京沪线车队」") || r.ByLLM {
		t.Fatalf("create: %+v", r)
	}
	code := st.AllGroups()[0].Code
	r = a.Handle(ctx, "friend", "加入房间"+mcp.SpokenCode(code))
	if !strings.HasPrefix(r.Text, "已加入京沪线车队") {
		t.Fatalf("join: %+v", r)
	}
	a.Handle(ctx, "friend", "建个群叫河南老乡群")
	r = a.Handle(ctx, "friend", "切到车队那个")
	if !strings.HasPrefix(r.Text, "已切换到京沪线车队") {
		t.Fatalf("switch: %+v", r)
	}
	if r := a.Handle(ctx, "friend", "今天天气怎么样"); r.Text != helpText {
		t.Fatalf("help: %+v", r)
	}
}

// fakeLLM requests switch_group on the first round and answers on the second.
func TestLLMFunctionCalling(t *testing.T) {
	srv, st := newMCP(t)
	st.CreateGroup("me", store.CreateGroupInput{Name: "京沪线车队"})
	st.CreateGroup("me", store.CreateGroupInput{Name: "河南老乡群"})

	round := 0
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []ai.Message     `json:"messages"`
			Tools    []map[string]any `json:"tools"`
		}
		json.Unmarshal(body, &req)
		round++
		var msg map[string]any
		if round == 1 {
			if len(req.Tools) < 10 || req.Messages[0].Role != "system" {
				http.Error(w, "missing tools/system", 400)
				return
			}
			msg = map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{
				"id": "call_1", "type": "function",
				"function": map[string]any{"name": "switch_group", "arguments": `{"query":"车队"}`},
			}}}
		} else {
			last := req.Messages[len(req.Messages)-1]
			if last.Role != "tool" || last.ToolCallID != "call_1" {
				http.Error(w, "missing tool result", 400)
				return
			}
			msg = map[string]any{"role": "assistant", "content": "好的，" + last.Content}
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": msg}}})
	}))
	defer llm.Close()

	a := &Assistant{MCP: srv, LLM: &ai.Client{BaseURL: llm.URL, Key: "k", Model: "m"}}
	r := a.Handle(context.Background(), "me", "帮我切回跑京沪的那个车队")
	if !r.ByLLM || len(r.Actions) != 1 || r.Actions[0].Tool != "switch_group" ||
		r.Text != "好的，已切换到京沪线车队，0人在线" {
		t.Fatalf("llm reply %+v", r)
	}

	// LLM outage → rules
	llm.Close()
	r = a.Handle(context.Background(), "me", "下一个频道")
	if r.ByLLM || !strings.HasPrefix(r.Text, "已切换到河南老乡群") {
		t.Fatalf("fallback %+v", r)
	}
}
