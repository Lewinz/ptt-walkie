package social

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/plant-squad/ptt-walkie/internal/store"
)

// HandlePairButton is the voice hub's OnPair hook.
func (s *Service) HandlePairButton(deviceID string) {
	if _, err := s.Pair(context.Background(), deviceID); errors.Is(err, ErrNoPartner) {
		s.Hub.Notify(deviceID, "info", "没有找到同时配对的设备，请两台设备一起按")
	}
}

func (s *Service) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/devices/{id}/pair", func(w http.ResponseWriter, r *http.Request) {
		g, err := s.Pair(r.Context(), r.PathValue("id"))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, g)
	})
	mux.HandleFunc("GET /api/devices/{id}/invites", func(w http.ResponseWriter, r *http.Request) {
		list := s.Pending(r.PathValue("id"))
		if list == nil {
			list = []Invite{}
		}
		writeJSON(w, http.StatusOK, list)
	})
	mux.HandleFunc("POST /api/devices/{id}/accept", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			GroupID string `json:"group_id"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		g, _, err := s.Accept(r.PathValue("id"), req.GroupID)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, g)
	})
	mux.HandleFunc("POST /api/groups/{id}/invite", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			From   string `json:"from"`
			Target string `json:"target"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if req.From == "" {
			req.From = r.Header.Get("X-Device-Id")
		}
		targets, err := s.Invite(req.From, r.PathValue("id"), req.Target)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"invited": targets})
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	switch {
	case errors.Is(err, ErrNoPartner), errors.Is(err, ErrNoInvite):
		code = http.StatusConflict
	case errors.Is(err, store.ErrNotFound):
		code = http.StatusNotFound
	case errors.Is(err, store.ErrNotMember):
		code = http.StatusForbidden
	case errors.Is(err, store.ErrInvalidInput):
		code = http.StatusBadRequest
	}
	writeJSON(w, code, map[string]string{"error": err.Error()})
}
