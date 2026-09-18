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

	maxErrors = 50
	queueSize = 256
)

func (s Status) finished() bool {
	return s == StatusDone || s == StatusFailed || s == StatusCancelled
}

// Source is the part of the Telegram client that jobs need.
type Source interface {
	Session() (*tg.Client, dcpool.Pool, error)
	Peer(ctx context.Context, ref string) (tgc.Chat, error)
	ListMediaIDs(ctx context.Context, chat tgc.Chat, filter string, progress func(int)) ([]int, error)
}

// Spec says what to download: explicit message ids, or every media message matching Filter.
type Spec struct {
	Ref    string
	IDs    []int
	All    bool
	Filter string
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
	v := View{
		ID: j.id, Title: j.title, Status: j.status, Message: j.message,
		Total: j.total, Done: j.done, Existing: j.existing, Failed: j.failed,
		BytesDone: j.bytes(), Speed: j.speed, Dest: j.dest,
		Active: []FileView{}, Errors: append([]string{}, j.errors...),
		CreatedAt: j.created.Unix(), seq: j.seq,
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

	mu    sync.Mutex
	jobs  map[string]*job
	seq   int
	queue chan string
}

func NewManager(src Source, settings func() config.Settings, log *zap.Logger) *Manager {
	return &Manager{src: src, settings: settings, log: log, jobs: map[string]*job{}, queue: make(chan string, queueSize)}
}

// Submit queues a job; the chat must be known (joined, or resolved from a link).
func (m *Manager) Submit(ctx context.Context, spec Spec) (View, error) {
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
	spec.IDs = dedupe(spec.IDs)
	dest := filepath.Join(m.settings().DownloadDir, fsname.ChatDir(chat.Title, spec.Ref))

	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	j := &job{
		id:      fmt.Sprintf("%d-%d", time.Now().Unix(), m.seq),
		seq:     m.seq,
		spec:    spec,
		title:   chat.Title,
		dest:    dest,
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
			m.end(j, StatusCancelled, "已取消")
		} else if j.cancel != nil {
			j.cancel()
		}
	}
	return j.view(), true
}

// RetrySpec returns a spec covering the files that failed in job id.
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
					m.end(j, StatusCancelled, "程序已退出")
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
			cancel()
			m.finish(j, err)
		}
	}
}

// end must be called with m.mu held.
func (m *Manager) end(j *job, s Status, msg string) {
	j.status, j.message, j.ended, j.speed, j.cancel = s, msg, time.Now(), 0, nil
}

func (m *Manager) finish(j *job, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case j.stopped, errors.Is(err, context.Canceled):
		m.end(j, StatusCancelled, "已取消，已下完的文件会保留")
	case err != nil:
		m.log.Warn("job failed", zap.String("job", j.id), zap.Error(err))
		m.end(j, StatusFailed, "失败："+err.Error())
	case j.failed > 0:
		m.end(j, StatusDone, fmt.Sprintf("完成，有 %d 个文件失败，可以点「重试失败的」", j.failed))
	default:
		m.end(j, StatusDone, "完成")
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
		ids, err = m.src.ListMediaIDs(ctx, chat, j.spec.Filter, func(n int) {
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

	s := m.settings()
	it := &iter{api: api, chat: chat, ids: ids, dir: j.dest, m: m, j: j}
	d := downloader.New(downloader.Options{Pool: pool, Threads: s.Threads, Iter: it, Progress: &progress{m: m, j: j}})

	stop := m.trackSpeed(j)
	err = d.Download(logctx.With(ctx, m.log.Named("dl")), s.Limit)
	stop()

	if ctx.Err() != nil {
		return ctx.Err() // user cancel or shutdown; finish() reports it
	}
	if it.fatal != nil {
		return it.fatal
	}
	return err
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
