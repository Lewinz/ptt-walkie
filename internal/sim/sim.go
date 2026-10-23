// Package sim implements headless walkie-talkie devices speaking the device
// protocol: an echo (radio-check) bot, a scripted talker and a load tester.
package sim

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/plant-squad/ptt-walkie/internal/archive"
)

// ---- device ----

type msg map[string]any

type device struct {
	id string
	ws *websocket.Conn
	mu sync.Mutex // serialises writes
}

func join(server, id, name, code string) error {
	body := strings.NewReader(fmt.Sprintf(`{"device_id":%q,"name":%q}`, id, name))
	resp, err := http.Post(strings.TrimRight(server, "/")+"/api/codes/"+url.PathEscape(code)+"/join", "application/json", body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("join %s: %s", code, resp.Status)
	}
	return nil
}

func dial(ctx context.Context, server, id, name string) (*device, error) {
	u, err := url.Parse(strings.TrimRight(server, "/") + "/ws")
	if err != nil {
		return nil, err
	}
	u.Scheme = strings.Replace(u.Scheme, "http", "ws", 1)
	q := u.Query()
	q.Set("device_id", id)
	q.Set("name", name)
	u.RawQuery = q.Encode()
	ws, _, err := websocket.Dial(ctx, u.String(), nil)
	if err != nil {
		return nil, err
	}
	ws.SetReadLimit(1 << 20)
	return &device{id: id, ws: ws}, nil
}

func (d *device) send(ctx context.Context, v any) error {
	b, _ := json.Marshal(v)
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.ws.Write(ctx, websocket.MessageText, b)
}

func (d *device) audio(ctx context.Context, pkt []byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.ws.Write(ctx, websocket.MessageBinary, pkt)
}

// read delivers text messages to onText and binary frames to onAudio.
func (d *device) read(ctx context.Context, onText func(msg), onAudio func([]byte)) error {
	for {
		typ, data, err := d.ws.Read(ctx)
		if err != nil {
			return err
		}
		if typ == websocket.MessageBinary {
			if onAudio != nil {
				onAudio(data)
			}
			continue
		}
		var m msg
		if json.Unmarshal(data, &m) == nil && onText != nil {
			onText(m)
		}
	}
}

// transmit takes the floor and sends packets at real-time pace.
func (d *device) transmit(ctx context.Context, granted <-chan bool, pkts [][]byte) error {
	// Drop any stale reply so we wait for the answer to this request.
	select {
	case <-granted:
	default:
	}
	if err := d.send(ctx, msg{"type": "ptt_start"}); err != nil {
		return err
	}
	select {
	case ok := <-granted:
		if !ok {
			return fmt.Errorf("floor denied")
		}
	case <-time.After(3 * time.Second):
		return fmt.Errorf("no floor response")
	case <-ctx.Done():
		return ctx.Err()
	}
	start := time.Now()
	var elapsed time.Duration
	for _, p := range pkts {
		if err := d.audio(ctx, p); err != nil {
			return err
		}
		elapsed += frameDur(p)
		time.Sleep(time.Until(start.Add(elapsed)))
	}
	return d.send(ctx, msg{"type": "ptt_stop"})
}

func frameDur(p []byte) time.Duration {
	if s := archive.PacketSamples(p); s > 0 {
		return time.Duration(s) * time.Second / 48000
	}
	return 60 * time.Millisecond
}

// ---- echo: radio check ----

// RunEcho runs a radio-check bot that repeats every transmission it hears.
func RunEcho(ctx context.Context, server, id, name, code string) error {
	if err := join(server, id, name, code); err != nil {
		return err
	}
	d, err := dial(ctx, server, id, name)
	if err != nil {
		return err
	}
	log.Printf("echo bot %s online in room %s: talk and it will repeat you", id, code)
	granted := make(chan bool, 1)
	var (
		mu  sync.Mutex
		buf [][]byte
	)
	return d.read(ctx, func(m msg) {
		switch m["type"] {
		case "floor":
			select {
			case granted <- m["granted"] == true:
			default:
			}
		case "talk_start":
			if m["replay"] == true {
				return
			}
			mu.Lock()
			buf = nil
			mu.Unlock()
			log.Printf("hearing %v", m["speaker_name"])
		case "talk_stop":
			if m["replay"] == true || m["reason"] == "preempted" {
				return
			}
			mu.Lock()
			pkts := buf
			buf = nil
			mu.Unlock()
			if len(pkts) < 3 {
				return
			}
			go func() {
				time.Sleep(600 * time.Millisecond)
				log.Printf("echoing %d frames", len(pkts))
				if err := d.transmit(ctx, granted, pkts); err != nil {
					log.Printf("echo: %v", err)
				}
			}()
		}
	}, func(p []byte) {
		mu.Lock()
		buf = append(buf, p)
		mu.Unlock()
	})
}

// ---- play: scripted chatter ----

// RunPlay transmits an Ogg Opus file every interval.
func RunPlay(ctx context.Context, server, id, name, code, file string, every time.Duration) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	pkts, err := archive.DecodeOggOpus(f)
	f.Close()
	if err != nil {
		return err
	}
	if err := join(server, id, name, code); err != nil {
		return err
	}
	d, err := dial(ctx, server, id, name)
	if err != nil {
		return err
	}
	granted := make(chan bool, 1)
	go d.read(ctx, func(m msg) {
		if m["type"] == "floor" {
			select {
			case granted <- m["granted"] == true:
			default:
			}
		}
	}, nil)
	log.Printf("%s will transmit %d frames every %s", id, len(pkts), every)
	for {
		if err := d.transmit(ctx, granted, pkts); err != nil {
			log.Printf("transmit: %v", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(every):
		}
	}
}

// ---- load: fan-out benchmark ----

// Load-test frames carry a send timestamp instead of Opus data; the relay
// forwards bytes opaquely, so receivers can compute one-way latency.
// LoadReport summarises a fan-out benchmark.
type LoadReport struct {
	Listeners, Frames   int
	Delivered, Expected int64
	P50, P90, P99, Max  time.Duration
}

func (r LoadReport) String() string {
	loss := 0.0
	if r.Expected > 0 {
		loss = 100 * float64(r.Expected-r.Delivered) / float64(r.Expected)
	}
	rd := func(d time.Duration) time.Duration { return d.Round(10 * time.Microsecond) }
	return fmt.Sprintf("listeners=%d frames=%d delivered=%d/%d (%.2f%% loss)\nlatency p50=%s p90=%s p99=%s max=%s",
		r.Listeners, r.Frames, r.Delivered, r.Expected, loss, rd(r.P50), rd(r.P90), rd(r.P99), rd(r.Max))
}

// RunLoad connects n listeners and one talker and measures relay latency.
func RunLoad(ctx context.Context, server, prefix, code string, n int, dur time.Duration) (LoadReport, error) {
	var (
		received atomic.Int64
		mu       sync.Mutex
		lat      []time.Duration
	)
	var listeners []*device
	for i := range n {
		id := fmt.Sprintf("%s-%03d", prefix, i)
		if err := join(server, id, id, code); err != nil {
			return LoadReport{}, err
		}
		d, err := dial(ctx, server, id, id)
		if err != nil {
			return LoadReport{}, err
		}
		listeners = append(listeners, d)
		go d.read(ctx, nil, func(p []byte) {
			if len(p) < 9 || p[0] != 0xFF {
				return
			}
			sent := time.Unix(0, int64(binary.BigEndian.Uint64(p[1:9])))
			received.Add(1)
			mu.Lock()
			lat = append(lat, time.Since(sent))
			mu.Unlock()
		})
	}
	talkerID := prefix + "-talker"
	if err := join(server, talkerID, "压测发言人", code); err != nil {
		return LoadReport{}, err
	}
	talker, err := dial(ctx, server, talkerID, "压测发言人")
	if err != nil {
		return LoadReport{}, err
	}
	granted := make(chan bool, 1)
	go talker.read(ctx, func(m msg) {
		if m["type"] == "floor" {
			select {
			case granted <- m["granted"] == true:
			default:
			}
		}
	}, nil)
	time.Sleep(500 * time.Millisecond)
	log.Printf("%d listeners connected; talking for %s", n, dur)

	frames := int(dur / (60 * time.Millisecond))
	if err := talker.send(ctx, msg{"type": "ptt_start"}); err != nil {
		return LoadReport{}, err
	}
	if ok := <-granted; !ok {
		return LoadReport{}, fmt.Errorf("floor denied")
	}
	tick := time.NewTicker(60 * time.Millisecond)
	for range frames {
		<-tick.C
		p := make([]byte, 60) // ~8 kbit/s Opus-sized payload
		p[0] = 0xFF
		binary.BigEndian.PutUint64(p[1:], uint64(time.Now().UnixNano()))
		if err := talker.audio(ctx, p); err != nil {
			return LoadReport{}, err
		}
	}
	tick.Stop()
	talker.send(ctx, msg{"type": "ptt_stop"})
	time.Sleep(time.Second)

	mu.Lock()
	defer mu.Unlock()
	expected := int64(frames * n)
	slices.Sort(lat)
	pct := func(p float64) time.Duration {
		if len(lat) == 0 {
			return 0
		}
		return lat[min(int(float64(len(lat))*p), len(lat)-1)]
	}
	rep := LoadReport{Listeners: n, Frames: frames, Delivered: received.Load(), Expected: expected,
		P50: pct(.5), P90: pct(.9), P99: pct(.99), Max: pct(1)}
	for _, d := range listeners {
		d.ws.Close(websocket.StatusNormalClosure, "")
	}
	return rep, nil
}
