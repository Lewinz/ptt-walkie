package mcp

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// HTTPHandler serves the MCP Streamable HTTP transport (JSON responses, no
// server-initiated stream). Device identity is taken from the
// /mcp/d/{device_id} path, the X-Device-Id header or the device_id query.
type HTTPHandler struct {
	Server *Server
	// Token, when set, must be presented as "Authorization: Bearer <token>"
	// or ?token=<token>.
	Token string
}

func (h *HTTPHandler) Register(mux *http.ServeMux) {
	mux.Handle("/mcp", h)
	mux.Handle("/mcp/d/{device}", h)
}

func (h *HTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.Token != "" && !h.authorized(r) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch r.Method {
	case http.MethodPost:
	case http.MethodDelete:
		w.WriteHeader(http.StatusNoContent)
		return
	default:
		// No standalone SSE stream: we never send server-initiated messages.
		w.Header().Set("Allow", "POST, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	caller := Caller{DeviceID: callerID(r)}

	var out []byte
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		// Legacy JSON-RPC batch (protocol 2025-03-26).
		var batch []json.RawMessage
		if err := json.Unmarshal(trimmed, &batch); err != nil {
			out = h.Server.Handle(r.Context(), caller, trimmed)
		} else {
			var resps []json.RawMessage
			for _, m := range batch {
				if resp := h.Server.Handle(r.Context(), caller, m); resp != nil {
					resps = append(resps, resp)
				}
			}
			if len(resps) > 0 {
				out, _ = json.Marshal(resps)
			}
		}
	} else {
		out = h.Server.Handle(r.Context(), caller, trimmed)
	}

	if bytes.Contains(trimmed, []byte(`"initialize"`)) && r.Header.Get("Mcp-Session-Id") == "" {
		b := make([]byte, 16)
		rand.Read(b)
		w.Header().Set("Mcp-Session-Id", hex.EncodeToString(b))
	}
	if out == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(out)
}

func (h *HTTPHandler) authorized(r *http.Request) bool {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if tok == "" {
		tok = r.URL.Query().Get("token")
	}
	return subtle.ConstantTimeCompare([]byte(tok), []byte(h.Token)) == 1
}

func callerID(r *http.Request) string {
	if id := r.PathValue("device"); id != "" {
		return id
	}
	if id := r.Header.Get("X-Device-Id"); id != "" {
		return id
	}
	return r.URL.Query().Get("device_id")
}
