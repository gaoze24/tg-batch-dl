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

func TestFileAccounting(t *testing.T) {
	m, dir := newTestManager(t)
	j := &job{id: "j", active: map[int64]*activeFile{}, status: StatusDownloading}
	mk := func(msgID int, size int64) *task {
		return &task{msgID: msgID, media: &tmedia.Media{Size: size}, final: filepath.Join(dir, fmt.Sprintf("%d.mp4", msgID)), date: 1700000000}
	}
	ctx := context.Background()

	// resumed file: 30 bytes were on disk before this attempt, so only 70 count as new
	ok := mk(10, 100)
	if err := os.WriteFile(ok.final, make([]byte, 100), 0o644); err != nil {
		t.Fatal(err)
	}
	a := m.startFile(j, ok)
	m.fileProgress(a, 30)
	m.fileProgress(a, 100)
	if got := j.bytes(); got != 70 {
		t.Errorf("in-flight bytes = %d, want 70", got)
	}
	m.endFile(ctx, j, ok, a, true, nil)
	if st, err := os.Stat(ok.final); err != nil || !st.ModTime().Equal(time.Unix(1700000000, 0)) {
		t.Errorf("mtime not set: %v %v", st, err)
	}

	bad := mk(11, 100)
	a = m.startFile(j, bad)
	m.fileProgress(a, 0)
	m.endFile(ctx, j, bad, a, false, errors.New("网络连接中断"))

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	stopped := mk(12, 100)
	a = m.startFile(j, stopped)
	m.endFile(cancelled, j, stopped, a, false, context.Canceled)

	if j.done != 1 || j.finished != 70 || j.failed != 1 || fmt.Sprint(j.failedID) != "[11]" || len(j.active) != 0 {
		t.Errorf("counters: done=%d finished=%d failed=%d ids=%v active=%d", j.done, j.finished, j.failed, j.failedID, len(j.active))
	}
	j.status = StatusDone
	m.jobs[j.id] = j
	if spec, err := m.RetrySpec("j"); err != nil || fmt.Sprint(spec.IDs) != "[11]" {
		t.Errorf("retry spec = %+v, %v", spec, err)
	}
}

func TestListerRemaining(t *testing.T) {
	l := &lister{ids: []int{1, 2, 3}, pos: 1}
	if fmt.Sprint(l.remaining()) != "[2 3]" {
		t.Errorf("remaining = %v", l.remaining())
	}
	l.pos = 3
	if l.remaining() != nil {
		t.Errorf("remaining after end = %v", l.remaining())
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
