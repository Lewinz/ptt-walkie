package kodo

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/plant-squad/ptt-walkie/internal/archive"
	"github.com/plant-squad/ptt-walkie/internal/voice"
)

func TestUploaderMirrorsOnce(t *testing.T) {
	var puts atomic.Int32
	var mu sync.Mutex
	objects := map[string]int{}
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			puts.Add(1)
			mu.Lock()
			objects[r.URL.Path] = int(r.ContentLength)
			mu.Unlock()
		}
	}))
	defer fake.Close()

	arc, _ := archive.Open(t.TempDir())
	up := &Uploader{Archive: arc, Prefix: "ptt/", Client: &Client{Endpoint: fake.URL, Region: "cn-east-1", Bucket: "b", AccessKey: "a", SecretKey: "s"}}
	up.Attach()

	u := voice.Utterance{ID: "u1", GroupID: "g1", StartedAt: time.Now(), Audio: voice.DefaultAudio}
	arc.Start(u)
	for range 4 {
		arc.Frame("u1", []byte{0x58, 1, 2})
	}
	arc.End(u)

	deadline := time.Now().Add(2 * time.Second)
	for {
		if r, _ := arc.Get("u1"); r.Remote != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("not mirrored")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// concurrent URL requests (ASR + download) must not re-upload
	rec, _ := arc.Get("u1")
	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			link, err := up.URL(context.Background(), rec)
			if err != nil || !strings.Contains(link, "/b/ptt/g1/u1.ogg?") {
				t.Errorf("url %s %v", link, err)
			}
		}()
	}
	wg.Wait()
	if puts.Load() != 1 {
		t.Fatalf("puts = %d", puts.Load())
	}
	if objects["/b/ptt/g1/u1.ogg"] == 0 {
		t.Fatalf("objects %v", objects)
	}
}
