package stats

import (
	"testing"
	"time"

	"github.com/plant-squad/ptt-walkie/internal/archive"
	"github.com/plant-squad/ptt-walkie/internal/store"
	"github.com/plant-squad/ptt-walkie/internal/voice"
)

func rec(group, speaker string, at time.Time, ms int64, alert ...string) archive.Record {
	return archive.Record{Utterance: voice.Utterance{ID: at.String() + speaker, GroupID: group, GroupName: group,
		SpeakerID: speaker, SpeakerName: speaker, StartedAt: at}, DurationMs: ms, Alert: alert}
}

func TestBuild(t *testing.T) {
	st, _ := store.New("")
	now := time.Date(2026, 10, 23, 15, 30, 0, 0, time.UTC)
	recs := []archive.Record{
		rec("g1", "a", now.Add(-26*time.Hour), 60000), // outside window
		rec("g1", "a", now.Add(-2*time.Hour), 30000),
		rec("g1", "b", now.Add(-2*time.Hour), 90000, "事故"),
		rec("g2", "a", now.Add(-10*time.Minute), 6000),
	}
	r := Build(recs, st, now, 24)
	if r.Totals.Utterances != 3 || r.Totals.ActiveDevices != 2 || r.Totals.ActiveGroups != 2 || r.Totals.Alerts != 1 {
		t.Fatalf("totals %+v", r.Totals)
	}
	if r.Totals.TalkMinutes != 2.1 {
		t.Fatalf("minutes %v", r.Totals.TalkMinutes)
	}
	if len(r.Hourly) != 24 || r.Hourly[23].Utterances != 1 || r.Hourly[21].Utterances != 2 {
		t.Fatalf("hourly %+v", r.Hourly[20:])
	}
	if r.Groups[0].ID != "g1" || r.Groups[0].Speakers != 2 || r.Groups[0].Alerts != 1 {
		t.Fatalf("groups %+v", r.Groups)
	}
	if r.Devices[0].ID != "b" {
		t.Fatalf("devices %+v", r.Devices)
	}
}
