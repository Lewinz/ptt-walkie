package kodo

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/plant-squad/ptt-walkie/internal/archive"
)

// Uploader mirrors every archived utterance to Kodo and hands out
// presigned URLs, so ASR works even when the server has no public address.
type Uploader struct {
	Client  *Client
	Archive *archive.Archive
	Prefix  string // object key prefix, e.g. "ptt-walkie/"

	locks sync.Map // utterance id → *sync.Mutex
}

// Attach uploads each new recording in the background.
func (u *Uploader) Attach() {
	u.Archive.OnSaved(func(rec archive.Record) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if _, err := u.Ensure(ctx, rec.ID); err != nil {
			slog.Warn("kodo upload", "utterance", rec.ID, "err", err)
		}
	})
}

// Ensure uploads the recording once and returns its object key.
func (u *Uploader) Ensure(ctx context.Context, id string) (string, error) {
	mu, _ := u.locks.LoadOrStore(id, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer func() {
		mu.(*sync.Mutex).Unlock()
		// Once uploaded, Remote short-circuits; the lock is no longer needed.
		if r, ok := u.Archive.Get(id); !ok || r.Remote != "" {
			u.locks.Delete(id)
		}
	}()

	rec, ok := u.Archive.Get(id)
	if !ok {
		return "", os.ErrNotExist
	}
	if rec.Remote != "" {
		return rec.Remote, nil
	}
	data, err := os.ReadFile(u.Archive.Path(rec))
	if err != nil {
		return "", err
	}
	key := strings.TrimLeft(u.Prefix, "/") + rec.File
	if err := u.Client.Put(ctx, key, "audio/ogg", data); err != nil {
		return "", err
	}
	u.Archive.Update(id, func(r *archive.Record) { r.Remote = key })
	slog.Info("kodo uploaded", "key", key, "bytes", len(data))
	return key, nil
}

// URL uploads if needed and returns a presigned download URL valid for 1h.
func (u *Uploader) URL(ctx context.Context, rec archive.Record) (string, error) {
	key, err := u.Ensure(ctx, rec.ID)
	if err != nil {
		return "", err
	}
	return u.Client.PresignGet(key, time.Hour)
}
