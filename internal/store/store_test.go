package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestParseCode(t *testing.T) {
	cases := map[string]string{
		"3721":      "3721",
		"加入房间3721":  "3721",
		"三七二幺":      "3721",
		"洞幺两拐":      "0127",
		"切到 ３８９２":   "3892",
		"老乡群":       "",
		"12":        "",
		"房间号是五六八九零": "56890",
	}
	for in, want := range cases {
		if got := ParseCode(in); got != want {
			t.Errorf("ParseCode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRandomCodeAvoidsConfusableDigits(t *testing.T) {
	for range 500 {
		c := randomCode(4)
		if len(c) != 4 || strings.ContainsAny(c, "17") || c[0] == '0' {
			t.Fatalf("bad code %q", c)
		}
	}
}

func TestCreateJoinSwitchLeave(t *testing.T) {
	s := newStore(t)
	var events []Event
	s.Subscribe(func(e Event) { events = append(events, e) })

	fleet, err := s.CreateGroup("dev-a", CreateGroupInput{Name: "京沪线车队"})
	if err != nil {
		t.Fatal(err)
	}
	if !fleet.IsLeader("dev-a") {
		t.Fatal("owner should be leader")
	}
	home, _ := s.CreateGroup("dev-a", CreateGroupInput{Name: "河南老乡群"})

	d, _ := s.Device("dev-a")
	if d.CurrentGroup != home.ID || len(d.Groups) != 2 {
		t.Fatalf("device state %+v", d)
	}

	if _, err := s.JoinGroup("dev-b", fleet.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SwitchGroup("dev-b", home.ID); err != ErrNotMember {
		t.Fatalf("switch to non-member group: %v", err)
	}

	// next / previous wraps around
	g, _ := s.CycleGroup("dev-a", 1)
	if g.ID != fleet.ID {
		t.Fatalf("cycle +1 → %s", g.Name)
	}
	g, _ = s.CycleGroup("dev-a", -1)
	if g.ID != home.ID {
		t.Fatalf("cycle -1 → %s", g.Name)
	}

	// owner leaves → leadership handed over, current falls back
	if err := s.LeaveGroup("dev-a", fleet.ID); err != nil {
		t.Fatal(err)
	}
	fleet, _ = s.Group(fleet.ID)
	if fleet.OwnerID != "dev-b" || !fleet.IsLeader("dev-b") {
		t.Fatalf("leadership not transferred: %+v", fleet)
	}

	var switched int
	for _, e := range events {
		if e.Type == EvSwitched {
			switched++
		}
	}
	if switched < 4 {
		t.Fatalf("expected switch events, got %+v", events)
	}
}

func TestResolve(t *testing.T) {
	s := newStore(t)
	fleet, _ := s.CreateGroup("me", CreateGroupInput{Name: "京沪线车队"})
	home, _ := s.CreateGroup("me", CreateGroupInput{Name: "河南老乡群"})
	other, _ := s.CreateGroup("someone", CreateGroupInput{Name: "别人的车队"})

	check := func(q, wantID string) {
		t.Helper()
		r := s.Resolve("me", q)
		if r.Group == nil || r.Group.ID != wantID {
			t.Errorf("Resolve(%q) = %+v, want %s", q, r, wantID)
		}
	}
	check("切到老乡群", home.ID)
	check("去车队那个", fleet.ID)
	check("京沪", fleet.ID)
	check(other.Code, other.ID) // codes resolve groups I'm not in

	if r := s.Resolve("me", "完全不相关"); r.Group != nil {
		t.Errorf("unexpected match %+v", r)
	}
}

func TestResolveAmbiguous(t *testing.T) {
	s := newStore(t)
	s.CreateGroup("me", CreateGroupInput{Name: "车队一"})
	s.CreateGroup("me", CreateGroupInput{Name: "车队二"})
	r := s.Resolve("me", "车队")
	if r.Group != nil || len(r.Candidates) != 2 {
		t.Fatalf("expected ambiguity, got %+v", r)
	}
}

func TestNearbyAndRoute(t *testing.T) {
	s := newStore(t)
	// 京沪高速 某服务区附近
	here := GeoPoint{Lat: 36.6512, Lng: 117.1201}
	near := GeoPoint{Lat: 36.6600, Lng: 117.1300} // ~1.3km
	far := GeoPoint{Lat: 39.9042, Lng: 116.4074}  // Beijing
	a, _ := s.CreateGroup("x", CreateGroupInput{Name: "服务区拼车", Type: GroupTemp, Visibility: VisNearby, Location: &near})
	s.CreateGroup("y", CreateGroupInput{Name: "北京群", Type: GroupTemp, Visibility: VisNearby, Location: &far})
	got := s.NearbyGroups(here, 3)
	if len(got) != 1 || got[0].ID != a.ID || got[0].DistanceKm < 1 || got[0].DistanceKm > 2 {
		t.Fatalf("nearby = %+v", got)
	}

	r, _ := s.CreateGroup("z", CreateGroupInput{Name: "冷链", Visibility: VisRoute, Route: "上海-成都 G42沪蓉高速"})
	if got := s.RouteGroups("沪蓉"); len(got) != 1 || got[0].ID != r.ID {
		t.Fatalf("route = %+v", got)
	}
}

func TestDissolveIfIdle(t *testing.T) {
	s := newStore(t)
	now := time.Now()
	s.now = func() time.Time { return now }
	tmp, _ := s.CreateGroup("a", CreateGroupInput{Name: "临时", Type: GroupTemp})
	fixed, _ := s.CreateGroup("a", CreateGroupInput{Name: "固定"})
	s.SwitchGroup("a", tmp.ID)

	now = now.Add(3 * time.Hour)
	tmp, _ = s.Group(tmp.ID)
	observed := tmp.LastActiveAt
	s.Touch(tmp.ID) // someone spoke after the janitor looked
	if s.DissolveIfIdle(tmp.ID, observed) {
		t.Fatal("dissolved a group that just became active")
	}
	tmp, _ = s.Group(tmp.ID)
	if s.DissolveIfIdle(fixed.ID, fixed.LastActiveAt) {
		t.Fatal("fixed groups are never dissolved")
	}
	if !s.DissolveIfIdle(tmp.ID, tmp.LastActiveAt) {
		t.Fatal("idle temp group not dissolved")
	}
	d, _ := s.Device("a")
	if d.CurrentGroup != fixed.ID || len(d.Groups) != 1 {
		t.Fatalf("device after dissolve %+v", d)
	}
	if _, err := s.GroupByCode(tmp.Code); err != ErrNotFound {
		t.Fatal("code should be released")
	}
}

func TestMuteAndPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, _ := New(path)
	g, _ := s.CreateGroup("a", CreateGroupInput{Name: "家人"})
	until := time.Now().Add(30 * time.Minute)
	if err := s.MuteGroup("a", g.ID, until); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}

	s2, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	d, _ := s2.Device("a")
	if !d.IsMuted(g.ID, time.Now()) {
		t.Fatal("mute not persisted")
	}
	if got, err := s2.GroupByCode(g.Code); err != nil || got.Name != "家人" {
		t.Fatalf("code index not rebuilt: %v", err)
	}
}

func TestFlushRetriesAfterFailure(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	os.WriteFile(blocker, nil, 0o644) // a file where a directory is needed
	s, _ := New(filepath.Join(dir, "state.json"))
	s.path = filepath.Join(blocker, "state.json") // unwritable
	s.CreateGroup("a", CreateGroupInput{Name: "车队"})
	if err := s.Flush(); err == nil {
		t.Fatal("expected write failure")
	}
	s.path = filepath.Join(dir, "state.json")
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	s2, _ := New(s.path)
	if len(s2.AllGroups()) != 1 {
		t.Fatal("changes lost after a failed flush")
	}
}
