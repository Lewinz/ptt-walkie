package voice

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/plant-squad/ptt-walkie/internal/api"
	"github.com/plant-squad/ptt-walkie/internal/store"
)

type joinMark struct {
	group string
	at    time.Time
}

// Hub owns all live device connections and per-group floor state.
//
// Locking: h.mu guards conns, floors and every Conn's stream/dev fields.
// Never call store mutators or api.Server.Status while holding h.mu — store
// mutators emit events back into the hub, and Status calls Presence methods.
type Hub struct {
	st        *store.Store
	api       *api.Server
	recorders []Recorder

	MaxTalk     time.Duration // hard cap per utterance
	IdleRelease time.Duration // release floor when no audio arrives for this long

	mu      sync.Mutex
	conns   map[string]*Conn
	floors  map[string]*Utterance // group id → active utterance
	offline map[string]time.Time  // device id → when it last disconnected
	joined  map[string]joinMark   // device id → group it just joined (greet on switch)
	now     func() time.Time

	// Hooks, set before serving.
	OnTranscript func(deviceID, utteranceID, text string)
	OnReconnect  func(deviceID string, offlineAt time.Time)
	OnPair       func(deviceID string) // "碰一碰" pair button pressed
}

func NewHub(st *store.Store, apiSrv *api.Server, maxTalk time.Duration) *Hub {
	h := &Hub{
		st:          st,
		api:         apiSrv,
		MaxTalk:     maxTalk,
		IdleRelease: 4 * time.Second,
		conns:       map[string]*Conn{},
		floors:      map[string]*Utterance{},
		offline:     map[string]time.Time{},
		joined:      map[string]joinMark{},
		now:         time.Now,
	}
	st.Subscribe(h.onEvent)
	return h
}

// AddRecorder registers an utterance sink (archive, transcription …).
func (h *Hub) AddRecorder(r Recorder) { h.recorders = append(h.recorders, r) }

// ---- api.Presence ----

func (h *Hub) Online(deviceID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.conns[deviceID]
	return ok
}

func (h *Hub) Speaker(groupID string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if u := h.floors[groupID]; u != nil {
		return u.SpeakerID
	}
	return ""
}

// ActiveUtterance returns a copy of the group's in-progress utterance.
func (h *Hub) ActiveUtterance(groupID string) (Utterance, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if u := h.floors[groupID]; u != nil {
		return *u, true
	}
	return Utterance{}, false
}

// ---- connection lifecycle ----

func (h *Hub) register(c *Conn) {
	h.mu.Lock()
	old := h.conns[c.id]
	h.conns[c.id] = c
	offlineAt, wasOffline := h.offline[c.id]
	delete(h.offline, c.id)
	h.mu.Unlock()
	if old != nil {
		old.sendJSON(noticeMsg{Type: MsgNotice, Level: "info", Text: "设备已在其他地方登录"})
		old.close()
		h.releaseHeldBy(old, "replaced")
	}
	slog.Info("device online", "device", c.id)
	h.sendWelcome(c)
	h.broadcastPresence(c.currentGroup())
	if wasOffline && h.OnReconnect != nil {
		go h.OnReconnect(c.id, offlineAt)
	}
}

func (h *Hub) unregister(c *Conn) {
	h.mu.Lock()
	current := h.conns[c.id] == c
	if current {
		delete(h.conns, c.id)
		h.offline[c.id] = h.now()
	}
	h.mu.Unlock()
	if !current {
		return // replaced by a newer session that now owns any floor
	}
	h.releaseHeldBy(c, "disconnected")
	slog.Info("device offline", "device", c.id)
	h.broadcastPresence(c.currentGroup())
}

func (h *Hub) sendWelcome(c *Conn) {
	ds, err := h.api.DeviceStatus(c.id)
	if err != nil {
		c.sendJSON(map[string]string{"type": MsgError, "message": err.Error()})
		return
	}
	c.sendJSON(map[string]any{
		"type":     MsgWelcome,
		"device":   ds,
		"audio":    DefaultAudio,
		"max_talk": h.MaxTalk.Milliseconds(),
		"announce": h.announce(ds.Current),
	})
}

// ---- floor control ----

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "u_" + hex.EncodeToString(b)
}

func (h *Hub) requestFloor(c *Conn) {
	dev, err := h.st.Device(c.id)
	if err != nil || dev.CurrentGroup == "" {
		c.sendJSON(floorMsg{Type: MsgFloor, Granted: false, Reason: "no_group"})
		return
	}
	g, err := h.st.Group(dev.CurrentGroup)
	if err != nil {
		c.sendJSON(floorMsg{Type: MsgFloor, Granted: false, Reason: "no_group"})
		return
	}
	leader := g.IsLeader(c.id)
	if g.TalkPolicy == store.TalkLeaders && !leader {
		c.sendJSON(floorMsg{Type: MsgFloor, Granted: false, GroupID: g.ID, Reason: "listen_only"})
		return
	}
	prio := PriorityNormal
	switch {
	case g.Type == store.GroupEmergency:
		prio = PriorityEmergency
	case leader:
		prio = PriorityLeader
	}

	h.mu.Lock()
	// Talking in a new group implicitly ends any stale transmission elsewhere.
	if u := h.heldByLocked(c.id); u != nil && u.GroupID != g.ID {
		h.endTalkLocked(u, "switched")
	}
	if cur := h.floors[g.ID]; cur != nil {
		if cur.SpeakerID == c.id {
			h.mu.Unlock()
			c.sendJSON(floorMsg{Type: MsgFloor, Granted: true, GroupID: g.ID, UtteranceID: cur.ID, MaxMs: h.MaxTalk.Milliseconds()})
			return
		}
		if prio <= cur.Priority {
			h.mu.Unlock()
			c.sendJSON(floorMsg{Type: MsgFloor, Granted: false, GroupID: g.ID, Reason: "busy",
				HolderID: cur.SpeakerID, HolderName: cur.SpeakerName})
			return
		}
		// Leader / emergency preempts a lower-priority speaker.
		if holder := h.conns[cur.SpeakerID]; holder != nil {
			holder.sendJSON(floorMsg{Type: MsgFloorRevoked, GroupID: g.ID, Reason: "preempted",
				HolderID: c.id, HolderName: dev.Name})
		}
		h.endTalkLocked(cur, "preempted")
	}
	now := h.now()
	u := &Utterance{
		ID: newID(), GroupID: g.ID, GroupName: g.Name,
		SpeakerID: c.id, SpeakerName: dev.Name, Leader: leader, Priority: prio,
		StartedAt: now, lastFrame: now, Audio: DefaultAudio,
	}
	h.floors[g.ID] = u
	for _, r := range h.recorders {
		r.Start(*u)
	}
	h.mu.Unlock()
	c.sendJSON(floorMsg{Type: MsgFloor, Granted: true, GroupID: g.ID, UtteranceID: u.ID, MaxMs: h.MaxTalk.Milliseconds()})
	h.st.Touch(g.ID)
}

func (h *Hub) releaseFloor(c *Conn) {
	h.releaseHeldBy(c, "released")
}

func (h *Hub) releaseHeldBy(c *Conn, reason string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if u := h.heldByLocked(c.id); u != nil {
		h.endTalkLocked(u, reason)
	}
}

func (h *Hub) heldByLocked(deviceID string) *Utterance {
	for _, u := range h.floors {
		if u.SpeakerID == deviceID {
			return u
		}
	}
	return nil
}

func (h *Hub) endTalkLocked(u *Utterance, reason string) {
	if h.floors[u.GroupID] == u {
		delete(h.floors, u.GroupID)
	}
	u.EndedAt = h.now()
	for _, c := range h.conns {
		if c.stream != nil && c.stream.ID == u.ID {
			c.sendJSON(talkStop(u, reason))
			c.stream = nil
		}
	}
	for _, r := range h.recorders {
		r.End(*u)
	}
	slog.Info("talk end", "group", u.GroupName, "speaker", u.SpeakerName,
		"frames", u.Frames, "dur", u.Duration().Round(time.Millisecond), "reason", reason)
}

// ---- audio fan-out ----

func (h *Hub) handleAudio(c *Conn, pkt []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	u := h.heldByLocked(c.id)
	if u == nil {
		return // not holding the floor: drop silently
	}
	u.Frames++
	u.lastFrame = h.now()
	for _, r := range h.recorders {
		r.Frame(u.ID, pkt)
	}
	now := h.now()
	for _, l := range h.conns {
		if l == c || !h.wantsLocked(l, u, now) {
			continue
		}
		h.deliverLocked(l, u, pkt)
	}
}

// wantsLocked decides whether listener l should hear utterance u.
func (h *Hub) wantsLocked(l *Conn, u *Utterance, now time.Time) bool {
	if l.dev.IsMuted(u.GroupID, now) {
		return false
	}
	if u.Priority < PriorityEmergency && h.heldByLocked(l.id) != nil {
		return false // half duplex: you can't hear while transmitting
	}
	if l.dev.CurrentGroup == u.GroupID {
		return !l.dev.LeaderOnly[u.GroupID] || u.Leader
	}
	// Emergency groups reach members even when tuned elsewhere.
	return u.Priority == PriorityEmergency && slices.Contains(l.dev.Groups, u.GroupID)
}

// deliverLocked sends one packet to l, switching its single downlink stream
// if u outranks what it is currently playing.
func (h *Hub) deliverLocked(l *Conn, u *Utterance, pkt []byte) {
	if l.stream == nil || l.stream.ID != u.ID {
		if l.stream != nil {
			if u.Priority <= l.stream.Priority {
				return
			}
			l.sendJSON(talkStop(l.stream, "preempted"))
		}
		l.stream = u
		l.sendJSON(talkStart(u))
	}
	l.sendAudio(pkt)
}

func talkStart(u *Utterance) talkMsg {
	return talkMsg{Type: MsgTalkStart, UtteranceID: u.ID, GroupID: u.GroupID, GroupName: u.GroupName,
		SpeakerID: u.SpeakerID, SpeakerName: u.SpeakerName, Leader: u.Leader, Priority: u.Priority,
		Replay: u.Priority == PriorityReplay}
}

func talkStop(u *Utterance, reason string) talkMsg {
	m := talkStart(u)
	m.Type = MsgTalkStop
	m.Reason = reason
	m.DurationMs = u.Duration().Milliseconds()
	return m
}

// Run enforces max talk time and releases floors abandoned mid-transmission.
func (h *Hub) Run(ctx context.Context) {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.sweep()
		}
	}
}

func (h *Hub) sweep() {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	for _, u := range h.floors {
		reason := ""
		switch {
		case h.MaxTalk > 0 && now.Sub(u.StartedAt) > h.MaxTalk:
			reason = "max_duration"
		case now.Sub(u.lastFrame) > h.IdleRelease:
			reason = "idle"
		default:
			continue
		}
		if c := h.conns[u.SpeakerID]; c != nil {
			c.sendJSON(floorMsg{Type: MsgFloorRevoked, GroupID: u.GroupID, Reason: reason})
		}
		h.endTalkLocked(u, reason)
	}
}

// ---- store events ----

func (h *Hub) onEvent(ev store.Event) {
	switch ev.Type {
	case store.EvSwitched:
		h.refresh(ev.DeviceID)
		h.mu.Lock()
		c := h.conns[ev.DeviceID]
		if c != nil {
			if u := h.heldByLocked(c.id); u != nil && u.GroupID != ev.GroupID {
				h.endTalkLocked(u, "switched")
			}
			if s := c.stream; s != nil && s.GroupID != ev.GroupID && s.Priority < PriorityEmergency {
				c.sendJSON(talkStop(s, "switched"))
				c.stream = nil
			}
		}
		h.mu.Unlock()
		if c != nil {
			h.sendChannel(c, ev.GroupID, "switched")
		}
		h.broadcastPresenceAll()
	case store.EvJoined:
		h.mu.Lock()
		// Greet only live joins; an offline join is just stale on reconnect.
		if h.conns[ev.DeviceID] != nil {
			h.joined[ev.DeviceID] = joinMark{group: ev.GroupID, at: h.now()}
		}
		h.mu.Unlock()
		h.refresh(ev.DeviceID)
		h.broadcastPresence(ev.GroupID)
	case store.EvLeft, store.EvUpdated:
		if ev.DeviceID != "" {
			h.refresh(ev.DeviceID)
		}
		if ev.GroupID != "" {
			h.broadcastPresence(ev.GroupID)
		}
	case store.EvDissolved:
		h.mu.Lock()
		if u := h.floors[ev.GroupID]; u != nil {
			h.endTalkLocked(u, "dissolved")
		}
		h.mu.Unlock()
		for _, id := range ev.Members {
			h.refresh(id)
			h.Notify(id, "info", fmt.Sprintf("%s已解散", ev.Name))
		}
	}
}

// refresh reloads the cached device view for a connected device.
func (h *Hub) refresh(deviceID string) {
	d, err := h.st.Device(deviceID)
	if err != nil {
		return
	}
	h.mu.Lock()
	if c := h.conns[deviceID]; c != nil {
		c.dev = d
	}
	h.mu.Unlock()
}

func (h *Hub) sendChannel(c *Conn, groupID, reason string) {
	var gs *api.GroupStatus
	if groupID != "" {
		if g, err := h.st.Group(groupID); err == nil {
			st := h.api.Status(g)
			gs = &st
		}
	}
	announce := h.announce(gs)
	h.mu.Lock()
	// Joins without a switch (e.g. batch import) leave a mark that must not
	// turn a much later manual switch into a "welcome".
	mark := h.joined[c.id]
	justJoined := groupID != "" && mark.group == groupID && h.now().Sub(mark.at) < 5*time.Second
	delete(h.joined, c.id)
	h.mu.Unlock()
	if justJoined && gs != nil {
		t := store.TemplateFor(gs.GroupView)
		announce = fmt.Sprintf(t.Greeting, gs.Name) + fmt.Sprintf("。%d人在线", gs.OnlineCount)
		if gs.TalkPolicy == store.TalkLeaders && !gs.IsLeader(c.id) {
			announce += "，本群只听不讲"
		}
		reason = "joined"
	}
	c.sendJSON(map[string]any{"type": MsgChannel, "group": gs, "reason": reason, "announce": announce})
}

// announce renders the short confirmation a device speaks after a switch.
func (h *Hub) announce(gs *api.GroupStatus) string {
	if gs == nil {
		return "当前未加入任何群"
	}
	return fmt.Sprintf("已切换到%s，%d人在线", gs.Name, gs.OnlineCount)
}

// Notify sends a short text notice (spoken by the device) to one device.
func (h *Hub) Notify(deviceID, level, text string) bool {
	h.mu.Lock()
	c := h.conns[deviceID]
	h.mu.Unlock()
	if c == nil {
		return false
	}
	c.sendJSON(noticeMsg{Type: MsgNotice, Level: level, Text: text})
	return true
}

// SendGroup sends an arbitrary JSON message to every device tuned to the
// group (and not muting it).
func (h *Hub) SendGroup(groupID string, msg any) {
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, c := range h.conns {
		if c.dev.CurrentGroup == groupID && !c.dev.IsMuted(groupID, h.now()) {
			c.sendRaw(data)
		}
	}
}

// Send delivers an arbitrary JSON message to one device.
func (h *Hub) Send(deviceID string, msg any) bool {
	h.mu.Lock()
	c := h.conns[deviceID]
	h.mu.Unlock()
	if c == nil {
		return false
	}
	c.sendJSON(msg)
	return true
}

// NotifyGroup sends a notice to every device tuned to the group.
func (h *Hub) NotifyGroup(groupID, level, text string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, c := range h.conns {
		if c.dev.CurrentGroup == groupID && !c.dev.IsMuted(groupID, h.now()) {
			c.sendJSON(noticeMsg{Type: MsgNotice, Level: level, Text: text, GroupID: groupID})
		}
	}
}

// SwitchNotify pushes the current channel to a device, e.g. after an MCP
// command that did not change state but should be confirmed.
func (h *Hub) SwitchNotify(deviceID string) {
	h.mu.Lock()
	c := h.conns[deviceID]
	h.mu.Unlock()
	if c != nil {
		h.sendChannel(c, c.currentGroup(), "refresh")
	}
}

func (h *Hub) broadcastPresenceAll() {
	h.mu.Lock()
	groups := map[string]bool{}
	for _, c := range h.conns {
		if c.dev.CurrentGroup != "" {
			groups[c.dev.CurrentGroup] = true
		}
	}
	h.mu.Unlock()
	for g := range groups {
		h.broadcastPresence(g)
	}
}

// broadcastPresence tells everyone tuned to the group who else is tuned in.
func (h *Hub) broadcastPresence(groupID string) {
	if groupID == "" {
		return
	}
	g, err := h.st.Group(groupID)
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	msg := presenceMsg{Type: MsgPresence, GroupID: groupID, MemberCount: len(g.Members), Online: []string{}}
	var tuned []*Conn
	for _, c := range h.conns {
		if c.dev.CurrentGroup == groupID {
			tuned = append(tuned, c)
			msg.Online = append(msg.Online, c.id)
		}
	}
	slices.Sort(msg.Online)
	msg.OnlineCount = len(msg.Online)
	data, _ := json.Marshal(msg)
	for _, c := range tuned {
		c.sendRaw(data)
	}
}
