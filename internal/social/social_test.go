package social

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/plant-squad/ptt-walkie/internal/store"
)

type fakeHub struct {
	mu      sync.Mutex
	notices map[string][]string
	sent    map[string][]any
}

func newHub() *fakeHub { return &fakeHub{notices: map[string][]string{}, sent: map[string][]any{}} }

func (f *fakeHub) Notify(id, _, text string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notices[id] = append(f.notices[id], text)
	return true
}

func (f *fakeHub) Send(id string, msg any) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent[id] = append(f.sent[id], msg)
	return true
}

func pairBoth(s *Service, a, b string) (store.GroupView, store.GroupView, error, error) {
	var ga, gb store.GroupView
	var ea, eb error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); ga, ea = s.Pair(context.Background(), a) }()
	time.Sleep(50 * time.Millisecond)
	go func() { defer wg.Done(); gb, eb = s.Pair(context.Background(), b) }()
	wg.Wait()
	return ga, gb, ea, eb
}

func TestPairSimultaneousPress(t *testing.T) {
	st, _ := store.New("")
	st.EnsureDevice("a", "老王")
	st.EnsureDevice("b", "小李")
	hub := newHub()
	s := &Service{Store: st, Hub: hub, Window: time.Second}

	ga, gb, ea, eb := pairBoth(s, "a", "b")
	if ea != nil || eb != nil || ga.ID == "" || ga.ID != gb.ID {
		t.Fatalf("pair: %v %v %s %s", ea, eb, ga.ID, gb.ID)
	}
	g, _ := st.Group(ga.ID)
	if g.Type != store.GroupTemp || len(g.Members) != 2 || g.Name != "老王和小李" {
		t.Fatalf("group %+v", g)
	}
	for _, id := range []string{"a", "b"} {
		if d, _ := st.Device(id); d.CurrentGroup != g.ID {
			t.Fatalf("%s not switched", id)
		}
		if len(hub.notices[id]) != 1 || !strings.Contains(hub.notices[id][0], "配对成功") {
			t.Fatalf("notice %v", hub.notices[id])
		}
	}
}

func TestPairRequiresProximityAndTiming(t *testing.T) {
	st, _ := store.New("")
	st.UpdateLocation("a", store.GeoPoint{Lat: 31.23, Lng: 121.47})
	st.UpdateLocation("b", store.GeoPoint{Lat: 39.90, Lng: 116.40}) // other city
	s := &Service{Store: st, Hub: newHub(), Window: 300 * time.Millisecond}
	_, _, ea, eb := pairBoth(s, "a", "b")
	if !errors.Is(ea, ErrNoPartner) || !errors.Is(eb, ErrNoPartner) {
		t.Fatalf("far devices paired: %v %v", ea, eb)
	}
	if len(st.AllGroups()) != 0 {
		t.Fatal("no group expected")
	}
}

func TestInviteByPlateAndAccept(t *testing.T) {
	st, _ := store.New("")
	hub := newHub()
	s := &Service{Store: st, Hub: hub}
	g, _ := st.CreateGroup("lead", store.CreateGroupInput{Name: "京沪线车队"})
	st.EnsureDevice("lead", "王队长")
	st.EnsureDevice("car-9", "沪A 12345")

	if _, err := s.Invite("stranger", g.ID, "沪A12345"); !errors.Is(err, store.ErrNotMember) {
		t.Fatalf("non-member invite: %v", err)
	}
	if _, err := s.Invite("lead", g.ID, "京B00000"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown plate: %v", err)
	}
	targets, err := s.Invite("lead", g.ID, "沪a12345")
	if err != nil || len(targets) != 1 || targets[0].ID != "car-9" {
		t.Fatalf("invite: %v %+v", err, targets)
	}
	if len(hub.sent["car-9"]) != 1 {
		t.Fatal("invite not pushed")
	}
	if p := s.Pending("car-9"); len(p) != 1 || p[0].FromName != "王队长" {
		t.Fatalf("pending %+v", p)
	}
	joined, inv, err := s.Accept("car-9", "")
	if err != nil || joined.ID != g.ID || inv.GroupName != "京沪线车队" {
		t.Fatalf("accept: %v", err)
	}
	if d, _ := st.Device("car-9"); d.CurrentGroup != g.ID {
		t.Fatal("not switched after accept")
	}
	if !strings.Contains(strings.Join(hub.notices["lead"], ""), "已接受邀请") {
		t.Fatalf("inviter not told: %v", hub.notices["lead"])
	}
	if _, _, err := s.Accept("car-9", ""); !errors.Is(err, ErrNoInvite) {
		t.Fatal("invite should be consumed")
	}
}
