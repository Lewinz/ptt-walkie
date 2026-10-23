// Package mcp exposes walkie-talkie control as Model Context Protocol tools
// so a Lingxi (灵矽) agent can create / join / switch groups, replay and
// summarise by voice.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
)

// Supported protocol revisions, newest first.
var protocolVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

// Instructions is the agent system prompt shared with the built-in assistant.
const Instructions = serverInstructions

const serverInstructions = `你是车载对讲机的语音助手。用户通过对讲机和车队、老乡、同路线司机群聊。
- 建群、入群、切群、查在线、静音、回放、摘要等一律调用工具完成，不要编造群名、房间号或在线人数。
- 房间号是 4~5 位数字，用户可能说「三八二五」「洞两拐」等，原样传给工具即可。
- 回复要非常简短（一句话），适合开车时收听；直接复述工具返回的文本即可。
- 切群名称有歧义时，把候选群名念给用户，让用户选择。`

// Caller identifies the device on whose behalf a tool runs.
type Caller struct {
	DeviceID string
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
)

// Tool is one MCP tool.
type Tool struct {
	Name        string         `json:"name"`
	Title       string         `json:"title,omitempty"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`

	handler func(ctx context.Context, c Caller, args Args) Result
}

// Result is a tool call outcome: Text is spoken back to the user by the
// agent; Data is attached as structuredContent.
type Result struct {
	Text    string
	Data    any
	IsError bool
}

func errResult(format string, a ...any) Result {
	return Result{Text: fmt.Sprintf(format, a...), IsError: true}
}

// Server dispatches JSON-RPC messages to tools. It is transport-agnostic.
type Server struct {
	tools []*Tool
	Name  string
}

func (s *Server) AddTool(t *Tool) { s.tools = append(s.tools, t) }

func (s *Server) Tools() []*Tool { return s.tools }

// Call invokes a tool directly (used by the built-in assistant).
func (s *Server) Call(ctx context.Context, caller Caller, name string, args map[string]any) (Result, bool) {
	i := slices.IndexFunc(s.tools, func(t *Tool) bool { return t.Name == name })
	if i < 0 {
		return Result{}, false
	}
	if args == nil {
		args = map[string]any{}
	}
	res := s.tools[i].handler(ctx, caller, Args(args))
	slog.Info("assistant tool", "tool", name, "device", caller.DeviceID, "error", res.IsError, "text", res.Text)
	return res, true
}

// Handle processes one JSON-RPC message and returns the encoded response, or
// nil for notifications.
func (s *Server) Handle(ctx context.Context, caller Caller, msg []byte) []byte {
	var req rpcRequest
	if err := json.Unmarshal(msg, &req); err != nil {
		return encode(rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"),
			Error: &rpcError{codeParse, "parse error"}})
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		if len(req.ID) == 0 {
			return nil // a response from the client, or junk: nothing to answer
		}
		return encode(rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{codeInvalidRequest, "invalid request"}})
	}
	result, rerr := s.dispatch(ctx, caller, req)
	if len(req.ID) == 0 {
		return nil // notification
	}
	resp := rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result, Error: rerr}
	if rerr == nil && result == nil {
		resp.Result = struct{}{}
	}
	return encode(resp)
}

func encode(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func (s *Server) dispatch(ctx context.Context, caller Caller, req rpcRequest) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(req.Params, &p)
		version := protocolVersions[0]
		if slices.Contains(protocolVersions, p.ProtocolVersion) {
			version = p.ProtocolVersion
		}
		return map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": s.Name, "title": "灵矽语音对讲机", "version": "0.1.0"},
			"instructions":    serverInstructions,
		}, nil
	case "ping":
		return struct{}{}, nil
	case "tools/list":
		return map[string]any{"tools": s.tools}, nil
	case "resources/list":
		return map[string]any{"resources": []any{}}, nil
	case "prompts/list":
		return map[string]any{"prompts": []any{}}, nil
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
			Meta      map[string]any `json:"_meta"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, &rpcError{codeInvalidParams, "invalid params"}
		}
		i := slices.IndexFunc(s.tools, func(t *Tool) bool { return t.Name == p.Name })
		if i < 0 {
			return nil, &rpcError{codeInvalidParams, "unknown tool: " + p.Name}
		}
		args := Args(p.Arguments)
		if args == nil {
			args = Args{}
		}
		// An explicit device_id (argument or _meta) only fills in a missing
		// identity; it never overrides the one bound by the transport, so a
		// caller (or a prompt-injected LLM) cannot act as another device.
		if caller.DeviceID == "" {
			if id := args.String("device_id"); id != "" {
				caller.DeviceID = id
			} else if id, _ := p.Meta["device_id"].(string); id != "" {
				caller.DeviceID = id
			}
		}
		res := s.tools[i].handler(ctx, caller, args)
		slog.Info("mcp tool", "tool", p.Name, "device", caller.DeviceID, "error", res.IsError, "text", res.Text)
		out := map[string]any{
			"content": []map[string]any{{"type": "text", "text": res.Text}},
			"isError": res.IsError,
		}
		if res.Data != nil {
			out["structuredContent"] = res.Data
		}
		return out, nil
	case "notifications/initialized", "notifications/cancelled":
		return nil, nil
	}
	return nil, &rpcError{codeMethodNotFound, "method not found: " + req.Method}
}

// Args wraps tool arguments with lenient accessors: LLMs send numbers as
// strings and vice versa.
type Args map[string]any

func (a Args) String(k string) string {
	switch v := a[k].(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	return ""
}

func (a Args) Int(k string, def int) int {
	switch v := a[k].(type) {
	case float64:
		return int(v)
	case string:
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n
		}
	}
	return def
}

func (a Args) Float(k string, def float64) float64 {
	switch v := a[k].(type) {
	case float64:
		return v
	case string:
		var f float64
		if _, err := fmt.Sscanf(v, "%g", &f); err == nil {
			return f
		}
	}
	return def
}

func (a Args) Bool(k string, def bool) bool {
	switch v := a[k].(type) {
	case bool:
		return v
	case string:
		switch v {
		case "true", "1", "yes", "是", "开", "打开":
			return true
		case "false", "0", "no", "否", "关", "关闭":
			return false
		}
	}
	return def
}
