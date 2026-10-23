// Package api exposes the group management REST API used by the admin H5,
// the device simulator and third-party integrations.
package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/skip2/go-qrcode"

	"github.com/plant-squad/ptt-walkie/internal/store"
)

// Presence reports live state from the voice plane. Implementations must be
// safe for concurrent use.
type Presence interface {
	Online(deviceID string) bool
	Speaker(groupID string) string
}

type noPresence struct{}

func (noPresence) Online(string) bool    { return false }
func (noPresence) Speaker(string) string { return "" }

type Server struct {
	Store          *store.Store
	Presence       Presence
	BaseURL        string
	NearbyRadiusKm float64
}

// MemberStatus is a group member annotated with live presence.
type MemberStatus struct {
	store.Member
	Name     string `json:"name"`
	Online   bool   `json:"online"` // connected to the voice plane
	Tuned    bool   `json:"tuned"`  // online and listening on this group
	Speaking bool   `json:"speaking"`
}

// GroupStatus is a group annotated with presence and share link.
type GroupStatus struct {
	store.GroupView
	Members     []MemberStatus `json:"members"`
	OnlineCount int            `json:"online_count"` // members tuned in right now
	Speaker     string         `json:"speaker,omitempty"`
	ShareURL    string         `json:"share_url"`
}

func (s *Server) presence() Presence {
	if s.Presence == nil {
		return noPresence{}
	}
	return s.Presence
}

// Status decorates a group with presence data.
func (s *Server) Status(g store.GroupView) GroupStatus {
	p := s.presence()
	st := GroupStatus{GroupView: g, Speaker: p.Speaker(g.ID), ShareURL: s.ShareURL(g.Code)}
	st.Members = make([]MemberStatus, 0, len(g.Members))
	for _, m := range g.Members {
		ms := MemberStatus{Member: m, Online: p.Online(m.DeviceID), Speaking: st.Speaker == m.DeviceID}
		if d, err := s.Store.Device(m.DeviceID); err == nil {
			ms.Name = d.Name
			ms.Tuned = ms.Online && d.CurrentGroup == g.ID
		}
		if ms.Tuned {
			st.OnlineCount++
		}
		st.Members = append(st.Members, ms)
	}
	return st
}

func (s *Server) ShareURL(code string) string {
	return strings.TrimRight(s.BaseURL, "/") + "/join/" + code
}

func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/groups", s.listGroups)
	mux.HandleFunc("POST /api/groups", s.createGroup)
	mux.HandleFunc("GET /api/groups/{id}", s.getGroup)
	mux.HandleFunc("DELETE /api/groups/{id}", s.dissolveGroup)
	mux.HandleFunc("GET /api/groups/{id}/qr.png", s.groupQR)
	mux.HandleFunc("POST /api/groups/{id}/join", s.joinGroup)
	mux.HandleFunc("POST /api/groups/{id}/leave", s.leaveGroup)
	mux.HandleFunc("POST /api/groups/{id}/mute", s.muteGroup)
	mux.HandleFunc("POST /api/groups/{id}/leader-only", s.leaderOnly)
	mux.HandleFunc("PUT /api/groups/{id}/members/{device}/role", s.setRole)
	mux.HandleFunc("GET /api/codes/{code}", s.groupByCode)
	mux.HandleFunc("POST /api/codes/{code}/join", s.joinByCode)
	mux.HandleFunc("GET /api/nearby", s.nearby)
	mux.HandleFunc("GET /api/routes", s.routes)
	mux.HandleFunc("GET /api/devices/{id}", s.getDevice)
	mux.HandleFunc("PUT /api/devices/{id}", s.updateDevice)
	mux.HandleFunc("POST /api/devices/{id}/location", s.updateLocation)
	mux.HandleFunc("POST /api/devices/{id}/switch", s.switchGroup)
}

// ---- handlers ----

func (s *Server) listGroups(w http.ResponseWriter, r *http.Request) {
	groups := s.Store.AllGroups()
	out := make([]GroupStatus, 0, len(groups))
	for _, g := range groups {
		out = append(out, s.Status(g))
	}
	writeJSON(w, http.StatusOK, out)
}

type createGroupReq struct {
	Name       string           `json:"name"`
	Type       store.GroupType  `json:"type"`
	Visibility store.Visibility `json:"visibility"`
	Route      string           `json:"route"`
	OwnerID    string           `json:"owner_id"`
	Lat        *float64         `json:"lat"`
	Lng        *float64         `json:"lng"`
}

func (s *Server) createGroup(w http.ResponseWriter, r *http.Request) {
	var req createGroupReq
	if !readJSON(w, r, &req) {
		return
	}
	in := store.CreateGroupInput{Name: req.Name, Type: req.Type, Visibility: req.Visibility, Route: req.Route}
	if req.Lat != nil && req.Lng != nil {
		in.Location = &store.GeoPoint{Lat: *req.Lat, Lng: *req.Lng}
	} else if req.Visibility == store.VisNearby && req.OwnerID != "" {
		if d, err := s.Store.Device(req.OwnerID); err == nil && d.Location != nil {
			in.Location = d.Location
		}
	}
	g, err := s.Store.CreateGroup(req.OwnerID, in)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, s.Status(g))
}

func (s *Server) getGroup(w http.ResponseWriter, r *http.Request) {
	g, err := s.Store.Group(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.Status(g))
}

func (s *Server) groupByCode(w http.ResponseWriter, r *http.Request) {
	g, err := s.Store.GroupByCode(store.ParseCode(r.PathValue("code")))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.Status(g))
}

func (s *Server) dissolveGroup(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	g, err := s.Store.Group(id)
	if err != nil {
		writeErr(w, err)
		return
	}
	// Only a leader may dissolve; an empty actor means the admin console.
	if actor := deviceID(r, ""); actor != "" && !g.IsLeader(actor) {
		writeErr(w, store.ErrForbidden)
		return
	}
	if err := s.Store.DissolveGroup(id); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) groupQR(w http.ResponseWriter, r *http.Request) {
	g, err := s.Store.Group(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	png, err := qrcode.Encode(s.ShareURL(g.Code), qrcode.Medium, 320)
	if err != nil {
		writeErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Write(png)
}

type deviceReq struct {
	DeviceID string `json:"device_id"`
	Name     string `json:"name"`
}

func (s *Server) joinGroup(w http.ResponseWriter, r *http.Request) {
	var req deviceReq
	if !readJSON(w, r, &req) {
		return
	}
	s.join(w, deviceID(r, req.DeviceID), req.Name, r.PathValue("id"))
}

func (s *Server) joinByCode(w http.ResponseWriter, r *http.Request) {
	var req deviceReq
	if !readJSON(w, r, &req) {
		return
	}
	g, err := s.Store.GroupByCode(store.ParseCode(r.PathValue("code")))
	if err != nil {
		writeErr(w, err)
		return
	}
	s.join(w, deviceID(r, req.DeviceID), req.Name, g.ID)
}

func (s *Server) join(w http.ResponseWriter, dev, name, groupID string) {
	if _, err := s.Store.EnsureDevice(dev, name); err != nil {
		writeErr(w, err)
		return
	}
	g, err := s.Store.JoinGroup(dev, groupID)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.Status(g))
}

func (s *Server) leaveGroup(w http.ResponseWriter, r *http.Request) {
	var req deviceReq
	if !readJSON(w, r, &req) {
		return
	}
	if err := s.Store.LeaveGroup(deviceID(r, req.DeviceID), r.PathValue("id")); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type muteReq struct {
	DeviceID string `json:"device_id"`
	Minutes  int    `json:"minutes"` // 0 = unmute, <0 = indefinitely
}

func (s *Server) muteGroup(w http.ResponseWriter, r *http.Request) {
	var req muteReq
	if !readJSON(w, r, &req) {
		return
	}
	until := MuteDeadline(time.Now(), req.Minutes)
	if err := s.Store.MuteGroup(deviceID(r, req.DeviceID), r.PathValue("id"), until); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"muted_until": until})
}

// MuteDeadline converts a minutes argument into a deadline: 0 unmutes,
// negative mutes for ten years.
func MuteDeadline(now time.Time, minutes int) time.Time {
	switch {
	case minutes == 0:
		return time.Time{}
	case minutes < 0:
		return now.AddDate(10, 0, 0)
	default:
		return now.Add(time.Duration(minutes) * time.Minute)
	}
}

func (s *Server) leaderOnly(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DeviceID string `json:"device_id"`
		On       bool   `json:"on"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if err := s.Store.SetLeaderOnly(deviceID(r, req.DeviceID), r.PathValue("id"), req.On); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) setRole(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ActorID string     `json:"actor_id"`
		Role    store.Role `json:"role"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Role != store.RoleLeader && req.Role != store.RoleMember {
		writeErr(w, store.ErrInvalidInput)
		return
	}
	if err := s.Store.SetRole(deviceID(r, req.ActorID), r.PathValue("id"), r.PathValue("device"), req.Role); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) nearby(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	lat, err1 := strconv.ParseFloat(q.Get("lat"), 64)
	lng, err2 := strconv.ParseFloat(q.Get("lng"), 64)
	if err1 != nil || err2 != nil {
		// Fall back to the device's last reported position.
		d, err := s.Store.Device(deviceID(r, ""))
		if err != nil || d.Location == nil {
			writeErr(w, errors.Join(store.ErrInvalidInput, errors.New("lat/lng or a located device_id required")))
			return
		}
		lat, lng = d.Location.Lat, d.Location.Lng
	}
	radius, err := strconv.ParseFloat(q.Get("radius_km"), 64)
	if err != nil || radius <= 0 {
		radius = s.NearbyRadiusKm
	}
	groups := s.Store.NearbyGroups(store.GeoPoint{Lat: lat, Lng: lng}, radius)
	out := make([]GroupStatus, 0, len(groups))
	for _, g := range groups {
		out = append(out, s.Status(g))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) routes(w http.ResponseWriter, r *http.Request) {
	groups := s.Store.RouteGroups(r.URL.Query().Get("q"))
	out := make([]GroupStatus, 0, len(groups))
	for _, g := range groups {
		out = append(out, s.Status(g))
	}
	writeJSON(w, http.StatusOK, out)
}

type DeviceStatus struct {
	store.DeviceView
	Online  bool          `json:"online"`
	Current *GroupStatus  `json:"current,omitempty"`
	List    []GroupStatus `json:"group_list"`
}

func (s *Server) DeviceStatus(id string) (DeviceStatus, error) {
	d, err := s.Store.Device(id)
	if err != nil {
		return DeviceStatus{}, err
	}
	out := DeviceStatus{DeviceView: d, Online: s.presence().Online(id)}
	for _, g := range s.Store.DeviceGroups(id) {
		st := s.Status(g)
		out.List = append(out.List, st)
		if g.ID == d.CurrentGroup {
			out.Current = &st
		}
	}
	return out, nil
}

func (s *Server) getDevice(w http.ResponseWriter, r *http.Request) {
	st, err := s.DeviceStatus(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) updateDevice(w http.ResponseWriter, r *http.Request) {
	var req deviceReq
	if !readJSON(w, r, &req) {
		return
	}
	d, err := s.Store.EnsureDevice(r.PathValue("id"), req.Name)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) updateLocation(w http.ResponseWriter, r *http.Request) {
	var p store.GeoPoint
	if !readJSON(w, r, &p) {
		return
	}
	if p.Lat < -90 || p.Lat > 90 || p.Lng < -180 || p.Lng > 180 {
		writeErr(w, store.ErrInvalidInput)
		return
	}
	s.Store.UpdateLocation(r.PathValue("id"), p)
	w.WriteHeader(http.StatusNoContent)
}

type switchReq struct {
	GroupID string `json:"group_id"`
	Query   string `json:"query"` // name or room code, fuzzy
	Delta   int    `json:"delta"` // +1 next, -1 previous
}

func (s *Server) switchGroup(w http.ResponseWriter, r *http.Request) {
	var req switchReq
	if !readJSON(w, r, &req) {
		return
	}
	g, candidates, err := s.Switch(r.PathValue("id"), req.GroupID, req.Query, req.Delta)
	if len(candidates) > 0 {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "ambiguous", "candidates": candidates})
		return
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.Status(g))
}

// Switch changes a device's current group by explicit id, by free-text query
// (name or room code; codes auto-join) or by stepping delta through its list.
// Ambiguous name queries return candidates and no error.
func (s *Server) Switch(dev, groupID, query string, delta int) (store.GroupView, []store.GroupView, error) {
	switch {
	case groupID != "":
		g, err := s.Store.SwitchGroup(dev, groupID)
		return g, nil, err
	case delta != 0:
		g, err := s.Store.CycleGroup(dev, delta)
		return g, nil, err
	case strings.TrimSpace(query) != "":
		res := s.Store.Resolve(dev, query)
		switch {
		case res.Group != nil && res.ByCode:
			g, err := s.Store.JoinGroup(dev, res.Group.ID)
			return g, nil, err
		case res.Group != nil:
			g, err := s.Store.SwitchGroup(dev, res.Group.ID)
			return g, nil, err
		case len(res.Candidates) > 0:
			return store.GroupView{}, res.Candidates, nil
		}
		return store.GroupView{}, nil, store.ErrNotFound
	}
	return store.GroupView{}, nil, store.ErrInvalidInput
}

// ---- helpers ----

// deviceID picks the acting device from the body, X-Device-Id header or
// device_id query parameter, in that order.
func deviceID(r *http.Request, body string) string {
	if body != "" {
		return body
	}
	if h := r.Header.Get("X-Device-Id"); h != "" {
		return h
	}
	return r.URL.Query().Get("device_id")
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.ContentLength == 0 {
		return true
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json: " + err.Error()})
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	switch {
	case errors.Is(err, store.ErrNotFound):
		code = http.StatusNotFound
	case errors.Is(err, store.ErrNotMember), errors.Is(err, store.ErrForbidden):
		code = http.StatusForbidden
	case errors.Is(err, store.ErrInvalidInput):
		code = http.StatusBadRequest
	}
	writeJSON(w, code, map[string]string{"error": err.Error()})
}
