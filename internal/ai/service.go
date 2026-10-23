package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/plant-squad/ptt-walkie/internal/archive"
	"github.com/plant-squad/ptt-walkie/internal/store"
	"github.com/plant-squad/ptt-walkie/internal/voice"
)

// Service wires transcription, keyword alerts, summaries and offline
// catch-up into the relay. It degrades gracefully: without an AI client it
// accepts client-side transcripts and summarises by speaker counts.
type Service struct {
	Client     *Client // nil when no AI key is configured
	Archive    *archive.Archive
	Replay     *archive.Handler
	Store      *store.Store
	Hub        *voice.Hub
	PublicURL  string // base URL the ASR service can fetch recordings from
	AlertWords []string

	// Reconnects after shorter outages than this are not worth a catch-up.
	CatchUpAfter time.Duration

	sem     chan struct{}
	mu      sync.Mutex
	pending map[string]pendingText // client transcripts that beat the archive
}

type pendingText struct {
	device, text string
	at           time.Time
}

// Attach registers the service's hooks. Call before serving.
func (s *Service) Attach() {
	s.sem = make(chan struct{}, 4)
	s.pending = map[string]pendingText{}
	if s.CatchUpAfter == 0 {
		s.CatchUpAfter = 20 * time.Second
	}
	s.Archive.OnSaved(s.onSaved)
	s.Hub.OnTranscript = s.onClientTranscript
	s.Hub.OnReconnect = s.onReconnect
}

// ServerASR reports whether recordings are transcribed server-side.
func (s *Service) ServerASR() bool { return s.Client != nil && s.Client.Key != "" }

func (s *Service) onSaved(rec archive.Record) {
	s.mu.Lock()
	p, ok := s.pending[rec.ID]
	delete(s.pending, rec.ID)
	s.mu.Unlock()
	if ok && p.device == rec.SpeakerID && !s.ServerASR() {
		s.apply(rec.ID, p.text)
		return
	}
	if !s.ServerASR() {
		return
	}
	s.sem <- struct{}{}
	defer func() { <-s.sem }()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	ogg, err := os.ReadFile(s.Archive.Path(rec))
	if err != nil {
		slog.Warn("asr read", "err", err)
		return
	}
	url := strings.TrimRight(s.PublicURL, "/") + "/api/recordings/" + rec.ID + "/audio.ogg"
	text, err := s.Client.Transcribe(ctx, url, ogg)
	if err != nil {
		slog.Warn("asr failed", "utterance", rec.ID, "err", err)
		return
	}
	if text != "" {
		s.apply(rec.ID, text)
	}
}

// onClientTranscript accepts the speaker's own transcript (browser Web
// Speech API) when no server-side ASR is configured.
func (s *Service) onClientTranscript(deviceID, utteranceID, text string) {
	if s.ServerASR() {
		return
	}
	text = strings.TrimSpace(text)
	if len([]rune(text)) > 500 {
		text = string([]rune(text)[:500])
	}
	if rec, ok := s.Archive.Get(utteranceID); ok {
		if rec.SpeakerID == deviceID {
			s.apply(utteranceID, text)
		}
		return
	}
	s.mu.Lock()
	now := time.Now()
	for id, p := range s.pending {
		if now.Sub(p.at) > time.Minute {
			delete(s.pending, id)
		}
	}
	s.pending[utteranceID] = pendingText{device: deviceID, text: text, at: now}
	s.mu.Unlock()
}

// MatchAlerts returns the alert keywords contained in text.
func MatchAlerts(text string, words []string) []string {
	var hits []string
	for _, w := range words {
		if w = strings.TrimSpace(w); w != "" && strings.Contains(text, w) {
			hits = append(hits, w)
		}
	}
	return hits
}

func (s *Service) apply(id, text string) {
	alerts := MatchAlerts(text, s.AlertWords)
	rec, err := s.Archive.Update(id, func(r *archive.Record) {
		r.Transcript = text
		r.Alert = alerts
	})
	if err != nil {
		return
	}
	slog.Info("transcript", "group", rec.GroupName, "speaker", rec.SpeakerName, "text", text, "alert", alerts)
	s.Hub.SendGroup(rec.GroupID, map[string]any{
		"type": "transcript", "utterance_id": rec.ID, "group_id": rec.GroupID,
		"speaker_id": rec.SpeakerID, "speaker_name": rec.SpeakerName, "text": text, "alert": alerts,
	})
	if len(alerts) == 0 {
		return
	}
	// Safety alerts reach every online member, even those tuned elsewhere.
	g, err := s.Store.Group(rec.GroupID)
	if err != nil {
		return
	}
	notice := fmt.Sprintf("注意！%s的%s说：%s", g.Name, rec.SpeakerName, text)
	now := time.Now()
	for _, m := range g.Members {
		if m.DeviceID == rec.SpeakerID {
			continue
		}
		if d, err := s.Store.Device(m.DeviceID); err == nil && !d.IsMuted(g.ID, now) {
			s.Hub.Notify(m.DeviceID, "alert", notice)
		}
	}
}

// ---- summaries ----

const summaryPrompt = `你是车队对讲机的语音助手。下面是司机错过的群语音转写记录。
请总结成一段给正在开车的司机听的简短播报：
- 不超过 80 个字，口语化，一口气能听完；
- 优先说路况、事故、查车、集合时间地点、需要回应的问题，并说明是谁说的；
- 闲聊一笔带过，不要编造记录里没有的信息。`

// Summary is the result of Summarize.
type Summary struct {
	Text       string           `json:"text"`
	GroupID    string           `json:"group_id"`
	GroupName  string           `json:"group_name"`
	Since      time.Time        `json:"since"`
	Count      int              `json:"count"`
	ByLLM      bool             `json:"by_llm"`
	Recordings []archive.Record `json:"recordings"`
}

func humanSince(since, now time.Time) string {
	d := now.Sub(since)
	switch {
	case d < time.Hour:
		return fmt.Sprintf("过去%d分钟", max(int(d.Minutes()+0.5), 1))
	case d < 24*time.Hour:
		return fmt.Sprintf("过去%d小时", int(d.Hours()+0.5))
	}
	return "自" + since.Format("1月2日15:04") + "以来"
}

// Summarize describes what others said in the group since the given time.
func (s *Service) Summarize(ctx context.Context, deviceID, groupID string, since time.Time) (Summary, error) {
	g, err := s.Store.Group(groupID)
	if err != nil {
		return Summary{}, err
	}
	now := time.Now()
	sum := Summary{GroupID: g.ID, GroupName: g.Name, Since: since, Recordings: []archive.Record{}}
	for _, r := range s.Archive.Since(groupID, since) {
		if r.SpeakerID != deviceID {
			sum.Recordings = append(sum.Recordings, r)
		}
	}
	sum.Count = len(sum.Recordings)
	when := humanSince(since, now)
	if sum.Count == 0 {
		sum.Text = fmt.Sprintf("%s%s没有新消息。", when, g.Name)
		return sum, nil
	}

	var lines []string
	for _, r := range sum.Recordings {
		if r.Transcript != "" {
			lines = append(lines, fmt.Sprintf("[%s] %s：%s", r.StartedAt.Format("15:04"), r.SpeakerName, r.Transcript))
		}
	}
	if len(lines) > 0 && s.ServerLLM() {
		if len(lines) > 60 {
			lines = lines[len(lines)-60:]
		}
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		text, err := s.Client.Chat(cctx, summaryPrompt, fmt.Sprintf("群：%s\n%s\n\n%s", g.Name, when, strings.Join(lines, "\n")))
		cancel()
		if err == nil && text != "" {
			sum.Text, sum.ByLLM = text, true
			return sum, nil
		}
		slog.Warn("summary llm failed", "err", err)
	}
	if len(lines) > 0 {
		recent := sum.Recordings
		if len(recent) > 5 {
			recent = recent[len(recent)-5:]
		}
		var parts []string
		for _, r := range recent {
			if r.Transcript != "" {
				parts = append(parts, fmt.Sprintf("%s说「%s」", r.SpeakerName, r.Transcript))
			}
		}
		sum.Text = fmt.Sprintf("%s%s有%d条消息，最近的：%s。", when, g.Name, sum.Count, strings.Join(parts, "；"))
		return sum, nil
	}
	// No text at all: summarise who spoke.
	counts := map[string]int{}
	var order []string
	for _, r := range sum.Recordings {
		if counts[r.SpeakerName] == 0 {
			order = append(order, r.SpeakerName)
		}
		counts[r.SpeakerName]++
	}
	sort.SliceStable(order, func(i, j int) bool { return counts[order[i]] > counts[order[j]] })
	var who []string
	for _, n := range order {
		who = append(who, fmt.Sprintf("%s%d条", n, counts[n]))
	}
	sum.Text = fmt.Sprintf("%s%s有%d条语音，来自%s。说「再放一遍」可以回放。", when, g.Name, sum.Count, strings.Join(who, "、"))
	return sum, nil
}

// ServerLLM reports whether an LLM is available for summaries.
func (s *Service) ServerLLM() bool {
	return s.Client != nil && s.Client.Key != "" && s.Client.Model != ""
}

// ---- offline catch-up ----

func (s *Service) onReconnect(deviceID string, offlineAt time.Time) {
	if time.Since(offlineAt) < s.CatchUpAfter {
		return
	}
	d, err := s.Store.Device(deviceID)
	if err != nil || d.CurrentGroup == "" {
		return
	}
	// Let the welcome / channel announcement play first.
	time.Sleep(2 * time.Second)
	sum, err := s.Summarize(context.Background(), deviceID, d.CurrentGroup, offlineAt)
	if err != nil || sum.Count == 0 {
		return
	}
	if sum.Count <= 3 {
		s.Hub.Notify(deviceID, "info", fmt.Sprintf("你离线期间%s有%d条语音，现在补播。", sum.GroupName, sum.Count))
		time.Sleep(3 * time.Second)
		if _, err := s.Replay.ReplayLast(deviceID, d.CurrentGroup, "", sum.Count); err != nil {
			slog.Debug("catch-up replay", "err", err)
		}
		return
	}
	s.Hub.Notify(deviceID, "info", "你离线期间的消息："+sum.Text)
}

// ---- HTTP ----

// Register exposes GET /api/groups/{id}/summary?minutes=&device_id=.
func (s *Service) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/groups/{id}/summary", func(w http.ResponseWriter, r *http.Request) {
		minutes, err := strconv.Atoi(r.URL.Query().Get("minutes"))
		if err != nil || minutes <= 0 {
			minutes = 30
		}
		dev := r.URL.Query().Get("device_id")
		if dev == "" {
			dev = r.Header.Get("X-Device-Id")
		}
		sum, err := s.Summarize(r.Context(), dev, r.PathValue("id"), time.Now().Add(-time.Duration(minutes)*time.Minute))
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if err != nil {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(sum)
	})
}
