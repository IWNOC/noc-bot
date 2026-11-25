package bot

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/IWNOC/noc-bot/internal/config"
	"github.com/IWNOC/noc-bot/internal/ingest"
	"github.com/IWNOC/noc-bot/internal/laya"
	"github.com/IWNOC/noc-bot/internal/notify"
	"github.com/IWNOC/noc-bot/internal/store"
)

// Every template reaches the line desk exactly once, including templates restored from before laya
// was connected; a template laya could not judge is tried again.
func TestEachTemplateJudgedOnce(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		json.NewEncoder(w).Encode(map[string]any{"answers": map[string]any{"odd": map[string]any{"answer": false, "source": "teacher"}}})
	}))
	defer srv.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.Open(filepath.Join(t.TempDir(), "b.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := &config.Config{}
	// first run without laya: the template is learned but not judged
	b, _ := New(cfg, log, st, nil, nil, notify.Log{L: log})
	l, _ := ingest.Parse("2026-11-25T19:00:00Z 10.20.0.2 eps-rt-02 devtime=2026-11-25T19:00:00Z vyos-hostsd[908]: Writing /etc/resolv.conf")
	b.Handle(l)
	snap, _ := b.Miner.Snapshot()
	st.Put(context.Background(), "drain", snap)

	// restart with laya: the restored template is sent once, then never again
	b2, _ := New(cfg, log, st, laya.New(srv.URL, "", 5*time.Second), nil, notify.Log{L: log})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b2.lineWorker(ctx)
	for i := 0; i < 5; i++ {
		b2.Handle(l)
	}
	deadline := time.Now().Add(3 * time.Second)
	for calls.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	if n := calls.Load(); n != 1 {
		t.Fatalf("line desk called %d times, want 1", n)
	}
	// a third start sees the stored decision and does not ask again
	b3, _ := New(cfg, log, st, laya.New(srv.URL, "", 5*time.Second), nil, notify.Log{L: log})
	go b3.lineWorker(ctx)
	b3.Handle(l)
	time.Sleep(300 * time.Millisecond)
	if n := calls.Load(); n != 1 {
		t.Fatalf("after restart the line desk was asked again (%d calls)", n)
	}
}
