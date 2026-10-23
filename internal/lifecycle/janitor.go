// Package lifecycle dissolves silent temporary groups, warning members
// shortly before it happens.
package lifecycle

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/plant-squad/ptt-walkie/internal/store"
)

// Notifier delivers spoken notices to devices tuned to a group.
type Notifier interface {
	NotifyGroup(groupID, level, text string)
	Speaker(groupID string) string
}

type Janitor struct {
	Store       *store.Store
	Hub         Notifier
	DefaultIdle time.Duration
	WarnBefore  time.Duration

	warned map[string]time.Time // group id → LastActiveAt we warned about
}

// Run sweeps every interval until ctx is done.
func (j *Janitor) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			j.Sweep(now)
		}
	}
}

// Sweep warns and dissolves as of now. It returns the dissolved group names.
func (j *Janitor) Sweep(now time.Time) []string {
	if j.warned == nil {
		j.warned = map[string]time.Time{}
	}
	if j.WarnBefore == 0 {
		j.WarnBefore = 5 * time.Minute
	}
	var dissolved []string
	live := map[string]bool{}
	for _, g := range j.Store.AllGroups() {
		if g.Type != store.GroupTemp {
			continue
		}
		live[g.ID] = true
		if j.Hub.Speaker(g.ID) != "" {
			continue // mid-transmission: obviously not idle
		}
		deadline := g.LastActiveAt.Add(store.IdleTimeout(g, j.DefaultIdle))
		switch {
		case now.After(deadline):
			if err := j.Store.DissolveGroup(g.ID); err == nil {
				slog.Info("temp group dissolved", "group", g.Name, "idle_since", g.LastActiveAt)
				dissolved = append(dissolved, g.Name)
			}
		case deadline.Sub(now) <= j.WarnBefore && !j.warned[g.ID].Equal(g.LastActiveAt):
			j.warned[g.ID] = g.LastActiveAt
			mins := max(int(deadline.Sub(now).Round(time.Minute).Minutes()), 1)
			j.Hub.NotifyGroup(g.ID, "info", fmt.Sprintf("临时群%s长时间没人说话，%d分钟后自动解散。说句话就能保留。", g.Name, mins))
		}
	}
	for id := range j.warned {
		if !live[id] {
			delete(j.warned, id)
		}
	}
	return dissolved
}
