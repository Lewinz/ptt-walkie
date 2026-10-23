// Package assistant is a stand-in for the Lingxi agent: it turns a spoken
// command into MCP tool calls, via LLM function calling when an AI key is
// configured, else via deterministic rules. It lets the browser simulator
// demo voice control end to end, and exercises the very same MCP tools.
package assistant

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/plant-squad/ptt-walkie/internal/ai"
	"github.com/plant-squad/ptt-walkie/internal/mcp"
)

type Assistant struct {
	MCP *mcp.Server
	LLM *ai.Client // optional
}

// Action is one executed tool call.
type Action struct {
	Tool    string         `json:"tool"`
	Args    map[string]any `json:"args"`
	Result  string         `json:"result"`
	IsError bool           `json:"is_error"`
}

type Reply struct {
	Text    string   `json:"text"`
	Actions []Action `json:"actions"`
	ByLLM   bool     `json:"by_llm"`
}

const helpText = "我没听懂。可以说：建个群叫某某、加入房间三八二五、切到老乡群、下一个频道、我错过了什么、再放一遍。"

// Handle executes a spoken command on behalf of a device.
func (a *Assistant) Handle(ctx context.Context, deviceID, text string) Reply {
	text = strings.TrimSpace(text)
	if text == "" {
		return Reply{Text: helpText, Actions: []Action{}}
	}
	caller := mcp.Caller{DeviceID: deviceID}
	if a.LLM != nil && a.LLM.Key != "" {
		if r, err := a.withLLM(ctx, caller, text); err == nil {
			return r
		} else {
			slog.Warn("assistant llm failed, using rules", "err", err)
		}
	}
	in, ok := ParseIntent(text)
	if !ok {
		return Reply{Text: helpText, Actions: []Action{}}
	}
	res, _ := a.MCP.Call(ctx, caller, in.Tool, in.Args)
	return Reply{Text: res.Text, Actions: []Action{{Tool: in.Tool, Args: in.Args, Result: res.Text, IsError: res.IsError}}}
}

func (a *Assistant) tools() []map[string]any {
	var out []map[string]any
	for _, t := range a.MCP.Tools() {
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": t.Name, "description": t.Description, "parameters": t.InputSchema,
			},
		})
	}
	return out
}

func (a *Assistant) withLLM(ctx context.Context, caller mcp.Caller, text string) (Reply, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	msgs := []ai.Message{
		{Role: "system", Content: mcp.Instructions},
		{Role: "user", Content: text},
	}
	tools := a.tools()
	reply := Reply{ByLLM: true, Actions: []Action{}}
	for range 4 {
		msg, err := a.LLM.ChatTools(ctx, msgs, tools)
		if err != nil {
			return Reply{}, err
		}
		if len(msg.ToolCalls) == 0 {
			reply.Text = strings.TrimSpace(msg.Content)
			if reply.Text == "" && len(reply.Actions) > 0 {
				reply.Text = reply.Actions[len(reply.Actions)-1].Result
			}
			return reply, nil
		}
		msgs = append(msgs, ai.Message{Role: "assistant", Content: msg.Content, ToolCalls: msg.ToolCalls})
		for _, tc := range msg.ToolCalls {
			var args map[string]any
			json.Unmarshal([]byte(tc.Function.Arguments), &args)
			res, ok := a.MCP.Call(ctx, caller, tc.Function.Name, args)
			if !ok {
				res = mcp.Result{Text: "未知工具 " + tc.Function.Name, IsError: true}
			}
			reply.Actions = append(reply.Actions, Action{Tool: tc.Function.Name, Args: args, Result: res.Text, IsError: res.IsError})
			msgs = append(msgs, ai.Message{Role: "tool", ToolCallID: tc.ID, Content: res.Text})
		}
	}
	if len(reply.Actions) > 0 {
		reply.Text = reply.Actions[len(reply.Actions)-1].Result
	}
	return reply, nil
}

// Register exposes POST /api/assistant {device_id, text}.
func (a *Assistant) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/assistant", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			DeviceID string `json:"device_id"`
			Text     string `json:"text"`
		}
		json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req)
		if req.DeviceID == "" {
			req.DeviceID = r.Header.Get("X-Device-Id")
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(a.Handle(r.Context(), req.DeviceID, req.Text))
	})
}
