package jobs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/tg"
	"github.com/iyear/tdl/core/dcpool"
	"github.com/iyear/tdl/core/downloader"
	"github.com/iyear/tdl/core/tmedia"
	"go.uber.org/zap"

	"github.com/gaoze24/tg-batch-dl/internal/config"
	"github.com/gaoze24/tg-batch-dl/internal/tgc"
)

type fakeSource struct {
	chats map[string]tgc.Chat
}

func (f fakeSource) Session() (*tg.Client, dcpool.Pool, error) { return nil, nil, tgc.ErrNotReady }

func (f fakeSource) Peer(_ context.Context, ref string) (tgc.Chat, error) {
	if ch, ok := f.chats[ref]; ok {
		return ch, nil
	}
	return tgc.Chat{}, tgc.ErrUnknownChat
}

func (f fakeSource) ListMediaIDs(context.Context, tgc.Chat, string, tgc.Range, func(int)) ([]int, error) {
	return nil, errors.New("not used")
}

func newTestManager(t *testing.T) (*Manager, string) {
	t.Helper()
	dir := t.TempDir()
	src := fakeSource{chats: map[string]tgc.Chat{"c1": {Ref: "c1", Title: "My: Channel"}}}
	settings := config.Defaults()
	settings.DownloadDir = dir
	return NewManager(src, func() config.Settings { return settings }, zap.NewNop(), ""), dir
}

func TestSubmitCancelClear(t *testing.T) {
	m, dir := newTestManager(t)
	v, err := m.Submit(context.Background(), Spec{Ref: "c1", IDs: []int{3, 1, 3, 0}})
	if err != nil {
		t.Fatal(err)
	}
	if v.Status != StatusQueued || v.Total != 2 || v.Title != "My: Channel" {
		t.Fatalf("unexpected view %+v", v)
	}
	if want := filepath.Join(dir, "My_ Channel"); v.Dest != want {
		t.Errorf("dest = %q, want %q", v.Dest, want)
	}
	c, ok := m.Cancel(v.ID)
	if !ok || c.Status != StatusCancelled {
		t.Fatalf("cancel: %+v %v", c, ok)
	}
	if n := m.ClearFinished(); n != 1 || len(m.Views()) != 0 {
		t.Errorf("clear removed %d, left %d", n, len(m.Views()))
	}
}

func TestSubmitValidation(t *testing.T) {
	m, _ := newTestManager(t)
	ctx := context.Background()
	if _, err := m.Submit(ctx, Spec{Ref: "c1"}); err == nil {
		t.Error("empty selection should fail")
	}
	if _, err := m.Submit(ctx, Spec{Ref: "c1", All: true, Filter: "nope"}); err == nil {
		t.Error("bad filter should fail")
	}
	if _, err := m.Submit(ctx, Spec{Ref: "c999", IDs: []int{1}}); !errors.Is(err, tgc.ErrUnknownChat) {
		t.Errorf("unknown chat: %v", err)
	}
}

func TestRunFailsWithoutSession(t *testing.T) {
	m, _ := newTestManager(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)
	v, err := m.Submit(ctx, Spec{Ref: "c1", IDs: []int{1}})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got := m.Views()[0]; got.Status == StatusFailed {
			if !strings.Contains(got.Message, tgc.ErrNotReady.Error()) {
				t.Errorf("message = %q", got.Message)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("job %s never failed: %+v", v.ID, m.Views())
}

func newElem(t *testing.T, dir string, id int64, msgID int, size int64, write int) *elem {
	t.Helper()
	final := filepath.Join(dir, "file"+string(rune('a'+id)))
	f, err := os.Create(final + partExt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(make([]byte, write)); err != nil {
		t.Fatal(err)
	}
	return &elem{id: id, msgID: msgID, media: &tmedia.Media{Size: size}, f: f, final: final, date: 1700000000}
}

func TestProgressOnDone(t *testing.T) {
	m, dir := newTestManager(t)
	j := &job{id: "j", active: map[int64]*activeFile{}, status: StatusDownloading}
	p := &progress{m: m, j: j}

	ok := newElem(t, dir, 1, 10, 100, 100)
	p.OnAdd(ok)
	p.OnDownload(ok, downloader.ProgressState{Downloaded: 100, Total: 100})
	p.OnDone(ok, nil)
	if _, err := os.Stat(ok.final); err != nil {
		t.Errorf("complete file not renamed: %v", err)
	}
	if st, _ := os.Stat(ok.final); st != nil && !st.ModTime().Equal(time.Unix(1700000000, 0)) {
		t.Errorf("mtime = %v", st.ModTime())
	}

	short := newElem(t, dir, 2, 11, 100, 40)
	p.OnAdd(short)
	p.OnDownload(short, downloader.ProgressState{Downloaded: 40, Total: 100})
	p.OnDone(short, nil) // tdl reports nil even when parts failed
	if _, err := os.Stat(short.final + partExt); !os.IsNotExist(err) {
		t.Errorf("incomplete part file should be removed: %v", err)
	}
	if _, err := os.Stat(short.final); !os.IsNotExist(err) {
		t.Errorf("incomplete file must not be published: %v", err)
	}

	// gotd reports a dropped connection as context.Canceled: that's a failure unless the user cancelled
	dropped := newElem(t, dir, 3, 12, 100, 10)
	p.OnAdd(dropped)
	p.OnDone(dropped, fmt.Errorf("rpcDoRequest: %w", context.Canceled))

	j.stopped = true
	userCancel := newElem(t, dir, 4, 13, 100, 10)
	p.OnAdd(userCancel)
	p.OnDone(userCancel, context.Canceled)

	if j.done != 1 || j.finished != 100 || j.failed != 2 || fmt.Sprint(j.failedID) != "[11 12]" {
		t.Errorf("counters: done=%d finished=%d failed=%d ids=%v", j.done, j.finished, j.failed, j.failedID)
	}
	if len(j.active) != 0 {
		t.Errorf("active not cleared: %v", j.active)
	}

	j.status = StatusDone
	m.jobs[j.id] = j
	spec, err := m.RetrySpec("j")
	if err != nil || fmt.Sprint(spec.IDs) != "[11 12]" {
		t.Errorf("retry spec = %+v, %v", spec, err)
	}
}

func TestPersistAndRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "jobs.json")
	src := fakeSource{chats: map[string]tgc.Chat{"c1": {Ref: "c1", Title: "Chan"}}}
	settings := config.Defaults()
	settings.DownloadDir = dir
	get := func() config.Settings { return settings }

	m1 := NewManager(src, get, zap.NewNop(), path)
	cancelled, err := m1.Submit(context.Background(), Spec{Ref: "c1", IDs: []int{5, 6, 7}})
	if err != nil {
		t.Fatal(err)
	}
	m1.Cancel(cancelled.ID)
	queued, err := m1.Submit(context.Background(), Spec{Ref: "c1", All: true, Filter: "video", Match: tgc.Range{MinSize: 1 << 20}})
	if err != nil {
		t.Fatal(err)
	}

	// "restart the program"
	m2 := NewManager(src, get, zap.NewNop(), path)
	views := m2.Views()
	if len(views) != 2 || views[0].ID != queued.ID || views[1].ID != cancelled.ID {
		t.Fatalf("restored views: %+v", views)
	}
	if views[0].Status != StatusInterrupted || !views[0].CanRestart {
		t.Errorf("unfinished job should come back interrupted and restartable: %+v", views[0])
	}
	if views[1].Status != StatusCancelled || !views[1].CanRestart {
		t.Errorf("cancelled job: %+v", views[1])
	}
	spec, err := m2.RestartSpec(cancelled.ID)
	if err != nil || fmt.Sprint(spec.IDs) != "[5 6 7]" || spec.Ref != "c1" {
		t.Errorf("restart spec = %+v, %v", spec, err)
	}
	spec, err = m2.RestartSpec(queued.ID)
	if err != nil || !spec.All || spec.Filter != "video" || spec.Match.MinSize != 1<<20 {
		t.Errorf("restart spec for all = %+v, %v", spec, err)
	}
	next, err := m2.Submit(context.Background(), spec)
	if err != nil || next.ID == queued.ID || next.ID == cancelled.ID {
		t.Errorf("resubmitted job %+v, %v (ids must not collide)", next, err)
	}
}

func TestMarkDownloaded(t *testing.T) {
	root := t.TempDir()
	chat := tgc.Chat{Ref: "c1", Title: "Chan"}
	dir := ChatDest(root, chat)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "1_a.mp4"), []byte("12345"), 0o644); err != nil {
		t.Fatal(err)
	}
	items := []tgc.MediaItem{
		{ID: 1, FileName: "1_a.mp4", Size: 5},
		{ID: 1, FileName: "1_a.mp4", Size: 9}, // same name, different size: partial or different file
		{ID: 2, FileName: "2_b.mp4", Size: 5},
	}
	MarkDownloaded(root, chat, items)
	if !items[0].Downloaded || items[1].Downloaded || items[2].Downloaded {
		t.Errorf("downloaded flags: %v %v %v", items[0].Downloaded, items[1].Downloaded, items[2].Downloaded)
	}
}

func TestIterRemaining(t *testing.T) {
	it := &iter{ids: []int{1, 2, 3}, pos: 1}
	if fmt.Sprint(it.remaining()) != "[2 3]" {
		t.Errorf("remaining = %v", it.remaining())
	}
	it.pos = 3
	if it.remaining() != nil {
		t.Errorf("remaining after end = %v", it.remaining())
	}
}
