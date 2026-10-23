package voice

import (
	"errors"
	"time"
)

var ErrOffline = errors.New("device offline")

// Replay streams archived packets to one device in real time. Each item is
// played in order; live talk on the device preempts and aborts the replay.
// It returns once playback has been scheduled.
func (h *Hub) Replay(deviceID string, items []ReplayItem) error {
	h.mu.Lock()
	c := h.conns[deviceID]
	h.mu.Unlock()
	if c == nil {
		return ErrOffline
	}
	go h.replay(c, items)
	return nil
}

// ReplayItem is one archived utterance to play back.
type ReplayItem struct {
	Utterance Utterance
	Packets   [][]byte
	FrameDur  func(pkt []byte) time.Duration
}

func (h *Hub) replay(c *Conn, items []ReplayItem) {
	for _, it := range items {
		u := it.Utterance
		u.ID = "r_" + u.ID
		u.Priority = PriorityReplay
		u.EndedAt = time.Time{}
		u.StartedAt = h.now()
		for _, pkt := range it.Packets {
			h.mu.Lock()
			if h.conns[c.id] != c {
				h.mu.Unlock()
				return
			}
			// Live talk owns the speaker: abort the whole replay.
			if c.stream != nil && c.stream.ID != u.ID {
				h.mu.Unlock()
				return
			}
			h.deliverLocked(c, &u, pkt)
			h.mu.Unlock()
			d := 60 * time.Millisecond
			if it.FrameDur != nil {
				if fd := it.FrameDur(pkt); fd > 0 {
					d = fd
				}
			}
			select {
			case <-time.After(d):
			case <-c.done:
				return
			}
		}
		h.mu.Lock()
		if c.stream != nil && c.stream.ID == u.ID {
			u.EndedAt = h.now()
			c.sendJSON(talkStop(&u, "replay_end"))
			c.stream = nil
		}
		h.mu.Unlock()
		// Short gap between consecutive utterances.
		time.Sleep(300 * time.Millisecond)
	}
}
