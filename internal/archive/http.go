package archive

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/plant-squad/ptt-walkie/internal/store"
	"github.com/plant-squad/ptt-walkie/internal/voice"
)

// Handler serves recording listing, download and on-device replay.
type Handler struct {
	Archive *Archive
	Store   *store.Store
	Hub     *voice.Hub
}

func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/groups/{id}/recordings", h.list)
	mux.HandleFunc("GET /api/recordings/{id}", h.get)
	mux.HandleFunc("GET /api/recordings/{id}/audio.ogg", h.audio)
	mux.HandleFunc("POST /api/devices/{id}/replay", h.replay)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	gid := r.PathValue("id")
	if since, err := time.Parse(time.RFC3339, q.Get("since")); err == nil {
		writeJSON(w, http.StatusOK, h.Archive.Since(gid, since))
		return
	}
	n, err := strconv.Atoi(q.Get("limit"))
	if err != nil || n <= 0 {
		n = 20
	}
	writeJSON(w, http.StatusOK, h.Archive.Recent(gid, n))
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	rec, ok := h.Archive.Get(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": errNotFound.Error()})
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

func (h *Handler) audio(w http.ResponseWriter, r *http.Request) {
	rec, ok := h.Archive.Get(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "audio/ogg")
	http.ServeFile(w, r, h.Archive.Path(rec))
}

type replayReq struct {
	GroupID     string `json:"group_id"`     // defaults to the device's current group
	N           int    `json:"n"`            // last n utterances, default 1
	UtteranceID string `json:"utterance_id"` // a specific recording
}

func (h *Handler) replay(w http.ResponseWriter, r *http.Request) {
	var req replayReq
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
	}
	recs, err := h.ReplayLast(r.PathValue("id"), req.GroupID, req.UtteranceID, req.N)
	if err != nil {
		code := http.StatusInternalServerError
		switch {
		case errors.Is(err, voice.ErrOffline), errors.Is(err, errNothing):
			code = http.StatusConflict
		case errors.Is(err, store.ErrNotFound), errors.Is(err, errNotFound):
			code = http.StatusNotFound
		}
		writeJSON(w, code, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, recs)
}

var errNothing = errors.New("nothing to replay")

// ReplayLast plays the last n utterances of a group (oldest first) on the
// device, excluding the device's own speech. It returns what will be played.
func (h *Handler) ReplayLast(deviceID, groupID, utteranceID string, n int) ([]Record, error) {
	var recs []Record
	if utteranceID != "" {
		rec, ok := h.Archive.Get(utteranceID)
		if !ok {
			return nil, errNotFound
		}
		recs = []Record{rec}
	} else {
		if groupID == "" {
			d, err := h.Store.Device(deviceID)
			if err != nil {
				return nil, err
			}
			groupID = d.CurrentGroup
		}
		if n <= 0 {
			n = 1
		}
		for _, rec := range h.Archive.Recent(groupID, n+20) {
			if rec.SpeakerID != deviceID {
				recs = append(recs, rec)
			}
			if len(recs) == n {
				break
			}
		}
		// Recent is newest first; play chronologically.
		for i, j := 0, len(recs)-1; i < j; i, j = i+1, j-1 {
			recs[i], recs[j] = recs[j], recs[i]
		}
	}
	if len(recs) == 0 {
		return nil, errNothing
	}
	items := make([]voice.ReplayItem, 0, len(recs))
	for _, rec := range recs {
		pkts, err := h.Archive.Packets(rec)
		if err != nil {
			return nil, err
		}
		items = append(items, voice.ReplayItem{Utterance: rec.Utterance, Packets: pkts, FrameDur: frameDur})
	}
	if err := h.Hub.Replay(deviceID, items); err != nil {
		return nil, err
	}
	return recs, nil
}

func frameDur(pkt []byte) time.Duration {
	return time.Duration(PacketSamples(pkt)) * time.Second / 48000
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
