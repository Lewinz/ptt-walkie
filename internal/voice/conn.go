package voice

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/plant-squad/ptt-walkie/internal/store"
)

const (
	sendQueue    = 256
	writeTimeout = 10 * time.Second
	pingInterval = 20 * time.Second
	maxMessage   = 64 << 10
)

type outFrame struct {
	text bool
	data []byte
}

// Conn is one device's WebSocket session.
type Conn struct {
	id  string
	hub *Hub
	ws  *websocket.Conn

	send      chan outFrame
	done      chan struct{}
	closeOnce sync.Once

	// Guarded by hub.mu.
	dev    store.DeviceView
	stream *Utterance // what this device's speaker is currently playing
}

func (c *Conn) currentGroup() string {
	c.hub.mu.Lock()
	defer c.hub.mu.Unlock()
	return c.dev.CurrentGroup
}

func (c *Conn) close() {
	c.closeOnce.Do(func() { close(c.done) })
}

func (c *Conn) sendJSON(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		slog.Error("marshal", "err", err)
		return
	}
	c.sendRaw(data)
}

func (c *Conn) sendRaw(data []byte) {
	select {
	case c.send <- outFrame{text: true, data: data}:
	case <-c.done:
	default:
		// A device that can't keep up with signalling is hopeless; drop it.
		slog.Warn("send queue full, closing", "device", c.id)
		c.close()
	}
}

func (c *Conn) sendAudio(pkt []byte) {
	select {
	case c.send <- outFrame{data: pkt}:
	default: // drop audio rather than stall the relay
	}
}

// ServeWS upgrades a device connection. Device identity comes from the
// device_id query parameter or X-Device-Id header.
func (h *Hub) ServeWS(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("device_id")
	if id == "" {
		id = r.Header.Get("X-Device-Id")
	}
	dev, err := h.st.EnsureDevice(id, r.URL.Query().Get("name"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	ws.SetReadLimit(maxMessage)
	c := &Conn{id: dev.ID, hub: h, ws: ws, dev: dev,
		send: make(chan outFrame, sendQueue), done: make(chan struct{})}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go c.writeLoop(ctx)
	h.register(c)
	defer h.unregister(c)
	c.readLoop(ctx)
	c.close()
}

func (c *Conn) writeLoop(ctx context.Context) {
	ping := time.NewTicker(pingInterval)
	defer ping.Stop()
	defer c.ws.CloseNow()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.done:
			c.ws.Close(websocket.StatusNormalClosure, "bye")
			return
		case f := <-c.send:
			typ := websocket.MessageBinary
			if f.text {
				typ = websocket.MessageText
			}
			wctx, cancel := context.WithTimeout(ctx, writeTimeout)
			err := c.ws.Write(wctx, typ, f.data)
			cancel()
			if err != nil {
				c.close()
				return
			}
		case <-ping.C:
			pctx, cancel := context.WithTimeout(ctx, writeTimeout)
			err := c.ws.Ping(pctx)
			cancel()
			if err != nil {
				c.close()
				return
			}
		}
	}
}

func (c *Conn) readLoop(ctx context.Context) {
	for {
		typ, data, err := c.ws.Read(ctx)
		if err != nil {
			if !errors.Is(err, context.Canceled) && websocket.CloseStatus(err) == -1 {
				slog.Debug("read", "device", c.id, "err", err)
			}
			return
		}
		select {
		case <-c.done:
			return
		default:
		}
		if typ == websocket.MessageBinary {
			c.hub.handleAudio(c, data)
			continue
		}
		var m Inbound
		if err := json.Unmarshal(data, &m); err != nil {
			c.sendJSON(map[string]string{"type": MsgError, "message": "invalid json"})
			continue
		}
		c.hub.handleMessage(c, m)
	}
}

func (h *Hub) handleMessage(c *Conn, m Inbound) {
	switch m.Type {
	case MsgHello:
		if m.Name != "" {
			h.st.EnsureDevice(c.id, m.Name)
			h.refresh(c.id)
		}
		h.sendWelcome(c)
	case MsgPTTStart:
		h.requestFloor(c)
	case MsgPTTStop:
		h.releaseFloor(c)
	case MsgSwitch:
		before := c.currentGroup()
		g, candidates, err := h.api.Switch(c.id, m.GroupID, m.Query, m.Delta)
		switch {
		case len(candidates) > 0:
			names := make([]string, len(candidates))
			for i, g := range candidates {
				names[i] = g.Name
			}
			c.sendJSON(map[string]any{"type": MsgError, "code": "ambiguous", "candidates": names,
				"message": "找到多个群，请说得更具体一些"})
		case errors.Is(err, store.ErrNotFound):
			c.sendJSON(map[string]string{"type": MsgError, "code": "not_found", "message": "没有找到这个群"})
		case err != nil:
			c.sendJSON(map[string]string{"type": MsgError, "message": err.Error()})
		case g.ID == before:
			// Switching to the current group emits no event; confirm anyway.
			h.SwitchNotify(c.id)
		}
	case MsgLocation:
		h.st.UpdateLocation(c.id, store.GeoPoint{Lat: m.Lat, Lng: m.Lng})
	case MsgTranscript:
		if h.OnTranscript != nil && m.UtteranceID != "" && m.Text != "" {
			h.OnTranscript(c.id, m.UtteranceID, m.Text)
		}
	case MsgPing:
		c.sendJSON(map[string]string{"type": MsgPong})
	default:
		c.sendJSON(map[string]string{"type": MsgError, "message": "unknown type " + m.Type})
	}
}
