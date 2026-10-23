// Package social implements the lighter ways of forming groups: "碰一碰"
// simultaneous-press pairing and inviting a specific device (e.g. by plate).
package social

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/plant-squad/ptt-walkie/internal/store"
)

// Notifier pushes messages to connected devices.
type Notifier interface {
	Notify(deviceID, level, text string) bool
	Send(deviceID string, msg any) bool
}

const (
	pairWindow   = 8 * time.Second
	pairRadiusKm = 0.3
	inviteTTL    = 30 * time.Minute
)

var (
	ErrNoPartner = errors.New("no partner pressed pair at the same time")
	ErrNoInvite  = errors.New("no pending invite")
)

type pairReq struct {
	device string
	at     time.Time
	loc    *store.GeoPoint
	result chan store.GroupView
}

type Invite struct {
	GroupID   string    `json:"group_id"`
	GroupName string    `json:"group_name"`
	Code      string    `json:"code"`
	FromID    string    `json:"from_id"`
	FromName  string    `json:"from_name"`
	At        time.Time `json:"at"`
}

type Service struct {
	Store *store.Store
	Hub   Notifier
	// Window is how close in time two presses must be (default 8s).
	Window time.Duration

	mu      sync.Mutex
	pending []*pairReq
	invites map[string][]Invite // target device → newest last
}

func (s *Service) window() time.Duration {
	if s.Window > 0 {
		return s.Window
	}
	return pairWindow
}

func (s *Service) init() {
	if s.invites == nil {
		s.invites = map[string][]Invite{}
	}
}

// ---- pairing ----

func near(a, b *store.GeoPoint) bool {
	if a == nil || b == nil {
		return true // without GPS on both sides, timing alone decides
	}
	return store.DistanceKm(*a, *b) <= pairRadiusKm
}

// Pair waits up to the pairing window for another device to press pair and
// then puts both into a fresh temporary group.
func (s *Service) Pair(ctx context.Context, deviceID string) (store.GroupView, error) {
	me, err := s.Store.EnsureDevice(deviceID, "")
	if err != nil {
		return store.GroupView{}, err
	}
	now := time.Now()
	s.mu.Lock()
	s.init()
	// Drop stale requests and any earlier press by this same device.
	live := s.pending[:0]
	var partner *pairReq
	for _, p := range s.pending {
		switch {
		case now.Sub(p.at) > s.window() || p.device == deviceID:
		case partner == nil && near(p.loc, me.Location):
			partner = p
		default:
			live = append(live, p)
		}
	}
	s.pending = live
	if partner == nil {
		req := &pairReq{device: deviceID, at: now, loc: me.Location, result: make(chan store.GroupView, 1)}
		s.pending = append(s.pending, req)
		s.mu.Unlock()
		select {
		case g := <-req.result:
			return g, nil
		case <-time.After(s.window()):
		case <-ctx.Done():
		}
		s.mu.Lock()
		for i, p := range s.pending {
			if p == req {
				s.pending = append(s.pending[:i], s.pending[i+1:]...)
				break
			}
		}
		s.mu.Unlock()
		// A partner may have matched us just as we gave up.
		select {
		case g := <-req.result:
			return g, nil
		default:
		}
		return store.GroupView{}, ErrNoPartner
	}
	s.mu.Unlock()

	other, _ := s.Store.Device(partner.device)
	g, err := s.Store.CreateGroup(partner.device, store.CreateGroupInput{
		Name: fmt.Sprintf("%s和%s", other.Name, me.Name), Type: store.GroupTemp,
	})
	if err != nil {
		return store.GroupView{}, err
	}
	if g, err = s.Store.JoinGroup(deviceID, g.ID); err != nil {
		return store.GroupView{}, err
	}
	partner.result <- g
	for _, id := range []string{partner.device, deviceID} {
		s.Hub.Notify(id, "info", fmt.Sprintf("配对成功，已进入临时对讲%s，房间号%s", g.Name, g.Code))
	}
	return g, nil
}

// ---- invites ----

// Invite asks target devices to join the group. It returns the invited
// devices (several when a name is ambiguous).
func (s *Service) Invite(fromID, groupID, target string) ([]store.DeviceView, error) {
	g, err := s.Store.Group(groupID)
	if err != nil {
		return nil, err
	}
	if !g.HasMember(fromID) {
		return nil, store.ErrNotMember
	}
	targets := s.Store.FindDevices(target)
	if len(targets) == 0 {
		return nil, store.ErrNotFound
	}
	from, _ := s.Store.Device(fromID)
	inv := Invite{GroupID: g.ID, GroupName: g.Name, Code: g.Code, FromID: fromID, FromName: from.Name, At: time.Now()}
	s.mu.Lock()
	s.init()
	for _, d := range targets {
		list := s.invites[d.ID]
		// one invite per group, newest wins
		kept := list[:0]
		for _, x := range list {
			if x.GroupID != g.ID && time.Since(x.At) < inviteTTL {
				kept = append(kept, x)
			}
		}
		s.invites[d.ID] = append(kept, inv)
	}
	s.mu.Unlock()
	for _, d := range targets {
		if g.HasMember(d.ID) {
			continue
		}
		s.Hub.Send(d.ID, map[string]any{"type": "invite", "invite": inv,
			"announce": fmt.Sprintf("%s邀请你加入%s，说「接受邀请」就能进群", from.Name, g.Name)})
	}
	return targets, nil
}

// Pending lists a device's unexpired invites, newest first.
func (s *Service) Pending(deviceID string) []Invite {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Invite
	list := s.invites[deviceID]
	for i := len(list) - 1; i >= 0; i-- {
		if time.Since(list[i].At) < inviteTTL {
			out = append(out, list[i])
		}
	}
	return out
}

// Accept joins the newest pending invite (or the one for groupID).
func (s *Service) Accept(deviceID, groupID string) (store.GroupView, Invite, error) {
	for _, inv := range s.Pending(deviceID) {
		if groupID != "" && inv.GroupID != groupID {
			continue
		}
		g, err := s.Store.JoinGroup(deviceID, inv.GroupID)
		if err != nil {
			continue // group dissolved meanwhile
		}
		s.mu.Lock()
		list := s.invites[deviceID]
		for i, x := range list {
			if x.GroupID == inv.GroupID {
				s.invites[deviceID] = append(list[:i], list[i+1:]...)
				break
			}
		}
		s.mu.Unlock()
		me, _ := s.Store.Device(deviceID)
		s.Hub.Notify(inv.FromID, "info", fmt.Sprintf("%s已接受邀请，加入了%s", me.Name, g.Name))
		return g, inv, nil
	}
	return store.GroupView{}, Invite{}, ErrNoInvite
}
