package lifecycle

import (
	"strings"
	"testing"
	"time"

	"github.com/plant-squad/ptt-walkie/internal/store"
)

type fakeHub struct {
	notices  []string
	speaking string
}

func (f *fakeHub) NotifyGroup(_, _, text string) { f.notices = append(f.notices, text) }
func (f *fakeHub) Speaker(g string) string {
	if g == f.speaking {
		return "someone"
	}
	return ""
}

func TestSweepWarnsThenDissolves(t *testing.T) {
	st, _ := store.New("")
	hub := &fakeHub{}
	j := &Janitor{Store: st, Hub: hub, DefaultIdle: time.Hour}

	tmp, _ := st.CreateGroup("a", store.CreateGroupInput{Name: "服务区", Type: store.GroupTemp})
	hike, _ := st.CreateGroup("a", store.CreateGroupInput{Name: "周末登山", Template: "hiking"}) // 24h idle
	st.CreateGroup("a", store.CreateGroupInput{Name: "车队"})                                  // fixed
	start := tmp.LastActiveAt

	if got := j.Sweep(start.Add(30 * time.Minute)); len(got) != 0 || len(hub.notices) != 0 {
		t.Fatalf("too early: %v %v", got, hub.notices)
	}
	j.Sweep(start.Add(57 * time.Minute))
	j.Sweep(start.Add(58 * time.Minute)) // warn only once
	if len(hub.notices) != 1 || !strings.Contains(hub.notices[0], "服务区") || !strings.Contains(hub.notices[0], "3分钟") {
		t.Fatalf("warning %v", hub.notices)
	}

	// someone is talking right now: keep it
	hub.speaking = tmp.ID
	if got := j.Sweep(start.Add(2 * time.Hour)); len(got) != 0 {
		t.Fatalf("dissolved while talking: %v", got)
	}
	hub.speaking = ""
	got := j.Sweep(start.Add(2 * time.Hour))
	if len(got) != 1 || got[0] != "服务区" {
		t.Fatalf("dissolved %v", got)
	}
	if _, err := st.Group(hike.ID); err != nil {
		t.Fatal("hiking template keeps 24h")
	}
	if len(st.AllGroups()) != 2 {
		t.Fatal("fixed group must survive")
	}
	if got := j.Sweep(start.Add(25 * time.Hour)); len(got) != 1 || got[0] != "周末登山" {
		t.Fatalf("hiking after 24h: %v", got)
	}
}
