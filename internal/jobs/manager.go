// Package jobs queues downloads and runs them one job at a time with tdl's core downloader.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/gotd/td/tg"
	"github.com/iyear/tdl/core/dcpool"
	"github.com/iyear/tdl/core/downloader"
	"github.com/iyear/tdl/core/logctx"
	"go.uber.org/zap"

	"github.com/gaoze24/tg-batch-dl/internal/config"
	"github.com/gaoze24/tg-batch-dl/internal/fsname"
	"github.com/gaoze24/tg-batch-dl/internal/tgc"
)

type Status string

const (
	StatusQueued      Status = "queued"
	StatusListing     Status = "listing"
	StatusDownloading Status = "downloading"
	StatusDone        Status = "done"
	StatusFailed      Status = "failed"
	StatusCancelled   Status = "cancelled"
	StatusInterrupted Status = "interrupted" // the program exited while the job was queued or running

	maxErrors  = 50
	queueSize  = 256
	autoPasses = 2 // first pass + one automatic retry of the files that failed
	retryPause = 5 * time.Second
)

func (s Status) finished() bool {
	return s == StatusDone || s == StatusFailed || s == StatusCancelled || s == StatusInterrupted
}

// Source is the part of the Telegram client that jobs need.
type Source interface {
	Session() (*tg.Client, dcpool.Pool, error)
	Peer(ctx context.Context, ref string) (tgc.Chat, error)
	ListMediaIDs(ctx context.Context, chat tgc.Chat, filter string, r tgc.Range, progress func(int)) ([]int, error)
}

// Spec says what to download: explicit message ids, or every media message matching Filter and Match.
type Spec struct {
	Ref    string
	IDs    []int
	All    bool
	Filter string
	Match  tgc.Range
}

type FileView struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	Done int64  `json:"done"`
}

type View struct {
	ID         string     `json:"id"`
	Title      string     `json:"title"`
	Status     Status     `json:"status"`
	Message    string     `json:"message,omitempty"`
	Total      int        `json:"total"`
	Done       int        `json:"done"`
	Existing   int        `json:"existing"`
	Failed     int        `json:"failed"`
	BytesDone  int64      `json:"bytes_done"`
	Speed      float64    `json:"speed"`
	Active     []FileView `json:"active"`
	Errors     []string   `json:"errors"`
	Dest       string     `json:"dest"`
	CreatedAt  int64      `json:"created_at"`
	FinishedAt int64      `json:"finished_at,omitempty"`
	CanRetry   bool       `json:"can_retry"`   // finished with failed files
	CanRestart bool       `json:"can_restart"` // finished without completing everything
	seq        int
}

type activeFile struct {
	name string
	size int64
	done int64
}

type job struct {
	id       string
	seq      int
	spec     Spec
	title    string
	dest     string
	status   Status
	message  string
	total    int
	done     int
	existing int
	failed   int
	finished int64 // bytes of completed files in this run
	speed    float64
	active   map[int64]*activeFile
	failedID []int
	errors   []string
	created  time.Time
	ended    time.Time
	cancel   context.CancelFunc
	stopped  bool
}

func (j *job) bytes() int64 {
	b := j.finished
	for _, a := range j.active {
		b += a.done
	}
	return b
}

func (j *job) view() View {
	fin := j.status.finished()
	v := View{
		ID: j.id, Title: j.title, Status: j.status, Message: j.message,
		Total: j.total, Done: j.done, Existing: j.existing, Failed: j.failed,
		BytesDone: j.bytes(), Speed: j.speed, Dest: j.dest,
		Active: []FileView{}, Errors: append([]string{}, j.errors...),
		CreatedAt:  j.created.Unix(),
		CanRetry:   fin && len(j.failedID) > 0,
		CanRestart: fin && j.status != StatusDone,
		seq:        j.seq,
	}
	if !j.ended.IsZero() {
		v.FinishedAt = j.ended.Unix()
	}
	for _, a := range j.active {
		v.Active = append(v.Active, FileView{Name: a.name, Size: a.size, Done: a.done})
	}
	sort.Slice(v.Active, func(a, b int) bool { return v.Active[a].Name < v.Active[b].Name })
	return v
}

type Manager struct {
	src      Source
	settings func() config.Settings
	log      *zap.Logger
	path     string // jobs.json; empty = don't persist

	mu    sync.Mutex
	jobs  map[string]*job
	seq   int
	queue chan string
}

// NewManager restores saved jobs from path (if any). Jobs that were unfinished when the program exited come back as
// "interrupted" so they can be restarted with one click.
func NewManager(src Source, settings func() config.Settings, log *zap.Logger, path string) *Manager {
	m := &Manager{src: src, settings: settings, log: log, path: path, jobs: map[string]*job{}, queue: make(chan string, queueSize)}
	m.load()
	return m
}

// ChatDest is the folder a chat's files are saved in.
func ChatDest(root string, chat tgc.Chat) string {
	return filepath.Join(root, fsname.ChatDir(chat.Title, chat.Ref))
}

// MarkDownloaded flags items whose file already exists with the expected size.
func MarkDownloaded(root string, chat tgc.Chat, items []tgc.MediaItem) {
	dir := ChatDest(root, chat)
	for i := range items {
		if st, err := os.Stat(filepath.Join(dir, items[i].FileName)); err == nil && st.Size() == items[i].Size {
			items[i].Downloaded = true
		}
	}
}

// Submit queues a job; the chat must be known (joined, or resolved from a link).
func (m *Manager) Submit(ctx context.Context, spec Spec) (View, error) {
	spec.IDs = dedupe(spec.IDs)
	if !spec.All && len(spec.IDs) == 0 {
		return View{}, errors.New("没有选择要下载的消息")
	}
	if spec.All && !tgc.ValidFilter(spec.Filter) {
		return View{}, errors.New("未知的媒体类型")
	}
	chat, err := m.src.Peer(ctx, spec.Ref)
	if err != nil {
		return View{}, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	j := &job{
		id:      fmt.Sprintf("%d-%d", time.Now().Unix(), m.seq),
		seq:     m.seq,
		spec:    spec,
		title:   chat.Title,
		dest:    ChatDest(m.settings().DownloadDir, chat),
		status:  StatusQueued,
		total:   len(spec.IDs),
		active:  map[int64]*activeFile{},
		created: time.Now(),
	}
	select {
	case m.queue <- j.id:
	default:
		return View{}, errors.New("排队的任务太多了，请稍后再试")
	}
	m.jobs[j.id] = j
	m.saveLocked()
	return j.view(), nil
}

func dedupe(ids []int) []int {
	seen := make(map[int]bool, len(ids))
	out := make([]int, 0, len(ids))
	for _, id := range ids {
		if id > 0 && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	sort.Ints(out)
	return out
}

// Views lists jobs, newest first.
func (m *Manager) Views() []View {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]View, 0, len(m.jobs))
	for _, j := range m.jobs {
		out = append(out, j.view())
	}
	sort.Slice(out, func(a, b int) bool { return out[a].seq > out[b].seq })
	return out
}

func (m *Manager) Cancel(id string) (View, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return View{}, false
	}
	if !j.status.finished() {
		j.stopped = true
		if j.status == StatusQueued {
			m.endLocked(j, StatusCancelled, "已取消")
		} else if j.cancel != nil {
			j.cancel()
		}
	}
	return j.view(), true
}

// RetrySpec covers only the files that failed in job id.
func (m *Manager) RetrySpec(id string) (Spec, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return Spec{}, errors.New("任务不存在")
	}
	if !j.status.finished() || len(j.failedID) == 0 {
		return Spec{}, errors.New("这个任务没有可重试的失败文件")
	}
	return Spec{Ref: j.spec.Ref, IDs: append([]int(nil), j.failedID...)}, nil
}

// RestartSpec repeats the whole job; files already on disk are skipped when it runs.
func (m *Manager) RestartSpec(id string) (Spec, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return Spec{}, errors.New("任务不存在")
	}
	if !j.status.finished() {
		return Spec{}, errors.New("任务还在进行中")
	}
	s := j.spec
	s.IDs = append([]int(nil), s.IDs...)
	return s, nil
}

func (m *Manager) Dest(id string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return "", false
	}
	return j.dest, true
}

func (m *Manager) ClearFinished() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for id, j := range m.jobs {
		if j.status.finished() {
			delete(m.jobs, id)
			n++
		}
	}
	m.saveLocked()
	return n
}

// Run processes the queue until ctx is done.
func (m *Manager) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			m.mu.Lock()
			for _, j := range m.jobs {
				if !j.status.finished() {
					m.endLocked(j, StatusInterrupted, "程序退出时中断了，可以点「重新开始」")
				}
			}
			m.mu.Unlock()
			return
		case id := <-m.queue:
			m.mu.Lock()
			j, ok := m.jobs[id]
			if !ok || j.status != StatusQueued {
				m.mu.Unlock()
				continue
			}
			jctx, cancel := context.WithCancel(ctx)
			j.cancel = cancel
			j.status = StatusListing
			m.mu.Unlock()

			err := m.run(jctx, j)
			shutdown := ctx.Err() != nil
			cancel()
			m.finish(j, err, shutdown)
		}
	}
}

// endLocked must be called with m.mu held.
func (m *Manager) endLocked(j *job, s Status, msg string) {
	j.status, j.message, j.ended, j.speed, j.cancel = s, msg, time.Now(), 0, nil
	m.saveLocked()
}

func (m *Manager) finish(j *job, err error, shutdown bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	defer func() {
		m.log.Info("job end", zap.String("job", j.id), zap.String("status", string(j.status)),
			zap.Int("done", j.done), zap.Int("existing", j.existing), zap.Int("failed", j.failed), zap.Ints("failed_ids", j.failedID))
	}()
	retryHint := ""
	if len(j.failedID) > 0 {
		retryHint = fmt.Sprintf("；%d 个文件没下成功，可以点「重试失败的」", len(j.failedID))
	}
	switch {
	case j.stopped:
		m.endLocked(j, StatusCancelled, "已取消。已下完的文件会保留，点「重新开始」可以接着下")
	case shutdown:
		m.endLocked(j, StatusInterrupted, "程序退出时中断了，可以点「重新开始」")
	case err != nil:
		m.log.Warn("job failed", zap.String("job", j.id), zap.Error(err))
		m.endLocked(j, StatusFailed, "失败："+err.Error()+retryHint)
	case j.failed > 0:
		m.endLocked(j, StatusDone, "完成"+retryHint)
	default:
		m.endLocked(j, StatusDone, "完成")
	}
}

func (m *Manager) run(ctx context.Context, j *job) error {
	api, pool, err := m.src.Session()
	if err != nil {
		return err
	}
	chat, err := m.src.Peer(ctx, j.spec.Ref)
	if err != nil {
		return err
	}
	ids := j.spec.IDs
	if j.spec.All {
		ids, err = m.src.ListMediaIDs(ctx, chat, j.spec.Filter, j.spec.Match, func(n int) {
			m.mu.Lock()
			j.total, j.message = n, fmt.Sprintf("正在列出媒体消息…已找到 %d 个", n)
			m.mu.Unlock()
		})
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("列出媒体消息失败: %w", err)
		}
	}
	if err := os.MkdirAll(j.dest, 0o755); err != nil {
		return fmt.Errorf("无法创建下载目录: %w", err)
	}

	m.mu.Lock()
	j.total, j.status, j.message = len(ids), StatusDownloading, ""
	m.mu.Unlock()
	m.log.Info("job start", zap.String("job", j.id), zap.String("chat", chat.Ref), zap.Int("files", len(ids)))

	stop := m.trackSpeed(j)
	defer stop()
	for pass := 1; ; pass++ {
		if err := m.pass(ctx, j, api, pool, chat, ids); err != nil {
			return err
		}
		if pass >= autoPasses {
			return nil
		}
		m.mu.Lock()
		retry := j.failedID
		if len(retry) > 0 {
			// forget this pass's failures; files that fail again are recorded again
			j.failedID, j.failed, j.errors = nil, 0, nil
			j.message = fmt.Sprintf("有 %d 个文件没下成功，稍后自动重试一次…", len(retry))
		}
		m.mu.Unlock()
		if len(retry) == 0 {
			return nil
		}
		t := time.NewTimer(retryPause)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
		m.mu.Lock()
		j.message = ""
		m.mu.Unlock()
		ids = retry
	}
}

// pass downloads ids once. Files that could not be downloaded are recorded on the job; only problems that make the
// whole job impossible are returned.
func (m *Manager) pass(ctx context.Context, j *job, api *tg.Client, pool dcpool.Pool, chat tgc.Chat, ids []int) error {
	s := m.settings()
	it := &iter{api: api, chat: chat, ids: ids, dir: j.dest, m: m, j: j}
	d := downloader.New(downloader.Options{Pool: pool, Threads: s.Threads, Iter: it, Progress: &progress{m: m, j: j}})
	err := d.Download(logctx.With(ctx, m.log.Named("dl")), s.Limit)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// tdl's downloader stops handing out files as soon as one reports a cancellation; those never started count as failed
	for _, id := range it.remaining() {
		m.fileFailed(j, id, "连接中断，还没开始下载")
	}
	if it.fatal != nil {
		return it.fatal
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

func (m *Manager) trackSpeed(j *job) func() {
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		m.mu.Lock()
		last := j.bytes()
		m.mu.Unlock()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				m.mu.Lock()
				cur := j.bytes()
				inst := float64(max(0, cur-last))
				j.speed = 0.6*j.speed + 0.4*inst
				last = cur
				m.mu.Unlock()
			}
		}
	}()
	return func() { close(done) }
}

// ---- per-file bookkeeping (called from iter and progress) ----

func (m *Manager) fileFailed(j *job, msgID int, reason string) {
	m.log.Warn("file failed", zap.String("job", j.id), zap.Int("msg", msgID), zap.String("reason", reason))
	m.mu.Lock()
	defer m.mu.Unlock()
	j.failed++
	j.failedID = append(j.failedID, msgID)
	if len(j.errors) < maxErrors {
		j.errors = append(j.errors, fmt.Sprintf("消息 #%d：%s", msgID, reason))
	}
}

func (m *Manager) fileExisting(j *job) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j.existing++
}

func (m *Manager) stopped(j *job) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return j.stopped
}
