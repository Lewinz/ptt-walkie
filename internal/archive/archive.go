// Package archive stores every PTT utterance as an Ogg Opus file with a
// JSON-lines index, and serves listing / download / replay.
package archive

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/plant-squad/ptt-walkie/internal/voice"
)

// Record is an archived utterance.
type Record struct {
	voice.Utterance
	DurationMs int64  `json:"duration_ms"`
	File       string `json:"file"` // relative to the archive dir
	Transcript string `json:"transcript,omitempty"`
	// Alert lists safety keywords found in the transcript ("事故", "封路" …).
	Alert []string `json:"alert,omitempty"`

	packets [][]byte // in-memory cache; nil when loaded from the index only
}

type pending struct {
	u       voice.Utterance
	packets [][]byte
	size    int
}

const (
	maxUtteranceBytes = 4 << 20
	// Utterances shorter than this are accidental key presses.
	minFrames = 3
	// Records kept in memory per group.
	keepPerGroup = 500
)

type Archive struct {
	dir string

	mu      sync.Mutex
	pending map[string]*pending
	byGroup map[string][]*Record // chronological
	byID    map[string]*Record
	index   *os.File

	subMu sync.RWMutex
	subs  []func(Record)
}

// Open loads (or creates) an archive rooted at dir.
func Open(dir string) (*Archive, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	a := &Archive{
		dir:     dir,
		pending: map[string]*pending{},
		byGroup: map[string][]*Record{},
		byID:    map[string]*Record{},
	}
	path := filepath.Join(dir, "index.jsonl")
	if f, err := os.Open(path); err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			var r Record
			if json.Unmarshal(sc.Bytes(), &r) != nil {
				continue
			}
			a.put(&r)
		}
		f.Close()
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	a.index = f
	return a, nil
}

func (a *Archive) Close() error { return a.index.Close() }

// put inserts or replaces a record (later index lines win).
func (a *Archive) put(r *Record) {
	if old, ok := a.byID[r.ID]; ok {
		*old = *r
		return
	}
	a.byID[r.ID] = r
	list := append(a.byGroup[r.GroupID], r)
	if len(list) > keepPerGroup {
		for _, gone := range list[:len(list)-keepPerGroup] {
			delete(a.byID, gone.ID)
		}
		list = slices.Clone(list[len(list)-keepPerGroup:])
	}
	a.byGroup[r.GroupID] = list
}

// OnSaved registers a callback run (in its own goroutine) for each new record.
func (a *Archive) OnSaved(fn func(Record)) {
	a.subMu.Lock()
	a.subs = append(a.subs, fn)
	a.subMu.Unlock()
}

// ---- voice.Recorder ----

func (a *Archive) Start(u voice.Utterance) {
	a.mu.Lock()
	a.pending[u.ID] = &pending{u: u}
	a.mu.Unlock()
}

func (a *Archive) Frame(id string, pkt []byte) {
	a.mu.Lock()
	defer a.mu.Unlock()
	p := a.pending[id]
	if p == nil || p.size+len(pkt) > maxUtteranceBytes {
		return
	}
	p.packets = append(p.packets, bytes.Clone(pkt))
	p.size += len(pkt)
}

func (a *Archive) End(u voice.Utterance) {
	a.mu.Lock()
	p := a.pending[u.ID]
	delete(a.pending, u.ID)
	if p == nil || len(p.packets) < minFrames {
		a.mu.Unlock()
		return
	}
	samples := 0
	for _, pkt := range p.packets {
		samples += PacketSamples(pkt)
	}
	r := &Record{
		Utterance:  u,
		DurationMs: int64(samples) / 48,
		File:       filepath.Join(u.GroupID, u.ID+".ogg"),
		packets:    p.packets,
	}
	if r.DurationMs == 0 {
		r.DurationMs = u.Duration().Milliseconds()
	}
	a.put(r)
	a.mu.Unlock()
	// Disk I/O and listeners run off the relay hot path.
	go a.persist(*r)
}

func (a *Archive) persist(r Record) {
	path := filepath.Join(a.dir, r.File)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		slog.Error("archive mkdir", "err", err)
		return
	}
	var buf bytes.Buffer
	if err := EncodeOggOpus(&buf, r.Audio.SampleRate, max(r.Audio.Channels, 1), r.packets); err != nil {
		slog.Error("archive encode", "err", err)
		return
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		slog.Error("archive write", "err", err)
		return
	}
	// A transcript may have been attached while we were encoding: index the
	// current version so the reloaded state is complete.
	if cur, ok := a.Get(r.ID); ok {
		r = cur
	}
	a.appendIndex(r)
	a.subMu.RLock()
	subs := slices.Clone(a.subs)
	a.subMu.RUnlock()
	for _, fn := range subs {
		fn(r)
	}
}

func (a *Archive) appendIndex(r Record) {
	line, _ := json.Marshal(r)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.index.Write(append(line, '\n'))
}

// Update mutates a record (e.g. adds a transcript) and persists the change.
func (a *Archive) Update(id string, fn func(*Record)) (Record, error) {
	a.mu.Lock()
	r, ok := a.byID[id]
	if !ok {
		a.mu.Unlock()
		return Record{}, errNotFound
	}
	fn(r)
	cp := *r
	a.mu.Unlock()
	a.appendIndex(cp)
	return cp, nil
}

var errNotFound = errors.New("recording not found")

// ---- queries ----

// Recent returns up to n records of the group, newest first.
func (a *Archive) Recent(groupID string, n int) []Record {
	a.mu.Lock()
	defer a.mu.Unlock()
	list := a.byGroup[groupID]
	var out []Record
	for i := len(list) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, *list[i])
	}
	return out
}

// Since returns the group's records started at or after t, oldest first.
func (a *Archive) Since(groupID string, t time.Time) []Record {
	a.mu.Lock()
	defer a.mu.Unlock()
	list := a.byGroup[groupID]
	i := sort.Search(len(list), func(i int) bool { return !list[i].StartedAt.Before(t) })
	out := make([]Record, 0, len(list)-i)
	for _, r := range list[i:] {
		out = append(out, *r)
	}
	return out
}

func (a *Archive) Get(id string) (Record, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	r, ok := a.byID[id]
	if !ok {
		return Record{}, false
	}
	return *r, true
}

// Path returns the absolute path of the record's audio file.
func (a *Archive) Path(r Record) string { return filepath.Join(a.dir, r.File) }

// Packets returns the raw Opus packets of a record.
func (a *Archive) Packets(r Record) ([][]byte, error) {
	if r.packets != nil {
		return r.packets, nil
	}
	f, err := os.Open(a.Path(r))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return DecodeOggOpus(f)
}
