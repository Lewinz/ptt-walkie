// Package stats aggregates talk activity for the fleet console (B 端报表).
package stats

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/plant-squad/ptt-walkie/internal/archive"
	"github.com/plant-squad/ptt-walkie/internal/store"
)

type Totals struct {
	Utterances    int     `json:"utterances"`
	TalkMinutes   float64 `json:"talk_minutes"`
	ActiveDevices int     `json:"active_devices"`
	ActiveGroups  int     `json:"active_groups"`
	Alerts        int     `json:"alerts"`
}

type Bucket struct {
	Start       time.Time `json:"start"`
	Utterances  int       `json:"utterances"`
	TalkMinutes float64   `json:"talk_minutes"`
}

type GroupStat struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Utterances  int       `json:"utterances"`
	TalkMinutes float64   `json:"talk_minutes"`
	Speakers    int       `json:"speakers"`
	Alerts      int       `json:"alerts"`
	Last        time.Time `json:"last"`
}

type DeviceStat struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Utterances  int     `json:"utterances"`
	TalkMinutes float64 `json:"talk_minutes"`
}

type Report struct {
	Since   time.Time    `json:"since"`
	Hours   int          `json:"hours"`
	Totals  Totals       `json:"totals"`
	Hourly  []Bucket     `json:"hourly"`
	Groups  []GroupStat  `json:"groups"`
	Devices []DeviceStat `json:"devices"` // top talkers
}

// Build aggregates the records of the last `hours` hours ending at now.
func Build(recs []archive.Record, st *store.Store, now time.Time, hours int) Report {
	end := now.Truncate(time.Hour).Add(time.Hour)
	since := end.Add(-time.Duration(hours) * time.Hour)
	rep := Report{Since: since, Hours: hours, Hourly: make([]Bucket, hours), Groups: []GroupStat{}, Devices: []DeviceStat{}}
	for i := range rep.Hourly {
		rep.Hourly[i].Start = since.Add(time.Duration(i) * time.Hour)
	}
	groups := map[string]*GroupStat{}
	speakers := map[string]map[string]bool{}
	devices := map[string]*DeviceStat{}
	for _, r := range recs {
		if r.StartedAt.Before(since) || !r.StartedAt.Before(end) {
			continue
		}
		mins := float64(r.DurationMs) / 60000
		rep.Totals.Utterances++
		rep.Totals.TalkMinutes += mins
		if len(r.Alert) > 0 {
			rep.Totals.Alerts++
		}
		b := &rep.Hourly[int(r.StartedAt.Sub(since)/time.Hour)]
		b.Utterances++
		b.TalkMinutes += mins

		g := groups[r.GroupID]
		if g == nil {
			g = &GroupStat{ID: r.GroupID, Name: r.GroupName}
			groups[r.GroupID] = g
			speakers[r.GroupID] = map[string]bool{}
		}
		g.Utterances++
		g.TalkMinutes += mins
		if len(r.Alert) > 0 {
			g.Alerts++
		}
		if r.StartedAt.After(g.Last) {
			g.Last = r.StartedAt
		}
		speakers[r.GroupID][r.SpeakerID] = true

		d := devices[r.SpeakerID]
		if d == nil {
			d = &DeviceStat{ID: r.SpeakerID, Name: r.SpeakerName}
			devices[r.SpeakerID] = d
		}
		d.Utterances++
		d.TalkMinutes += mins
	}
	for id, g := range groups {
		g.Speakers = len(speakers[id])
		if cur, err := st.Group(id); err == nil {
			g.Name = cur.Name
		}
		rep.Groups = append(rep.Groups, *g)
	}
	for _, d := range devices {
		if cur, err := st.Device(d.ID); err == nil {
			d.Name = cur.Name
		}
		rep.Devices = append(rep.Devices, *d)
	}
	rep.Totals.ActiveGroups = len(groups)
	rep.Totals.ActiveDevices = len(devices)
	sort.Slice(rep.Groups, func(i, j int) bool { return rep.Groups[i].TalkMinutes > rep.Groups[j].TalkMinutes })
	sort.Slice(rep.Devices, func(i, j int) bool { return rep.Devices[i].TalkMinutes > rep.Devices[j].TalkMinutes })
	if len(rep.Devices) > 10 {
		rep.Devices = rep.Devices[:10]
	}
	return rep
}

// Register exposes GET /api/stats?hours=24.
func Register(mux *http.ServeMux, arc *archive.Archive, st *store.Store) {
	mux.HandleFunc("GET /api/stats", func(w http.ResponseWriter, r *http.Request) {
		hours, err := strconv.Atoi(r.URL.Query().Get("hours"))
		if err != nil || hours <= 0 || hours > 24*7 {
			hours = 24
		}
		now := time.Now()
		rep := Build(arc.All(now.Add(-time.Duration(hours+1)*time.Hour)), st, now, hours)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(rep)
	})
}
