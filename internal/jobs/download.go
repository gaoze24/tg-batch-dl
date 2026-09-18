package jobs

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/gotd/td/tg"
	"github.com/iyear/tdl/core/dcpool"
	"github.com/iyear/tdl/core/util/tutil"
	"go.uber.org/zap"

	"github.com/gaoze24/tg-batch-dl/internal/tgc"
)

// downloadOne fetches one file and records the outcome on the job.
func (m *Manager) downloadOne(ctx context.Context, j *job, api *tg.Client, pool dcpool.Pool, chat tgc.Chat, t *task, threads int) {
	a := m.startFile(j, t)
	fetch := m.fetcher(api, pool, chat.InputPeer(), t.msgID, t.media)
	threads = tutil.BestThreads(t.media.Size, threads) // fewer connections for small files, like tdl
	resumed, err := downloadFile(ctx, t.final, t.media.Size, t.key(), threads, fetch, func(done int64) { m.fileProgress(a, done) })
	m.endFile(ctx, j, t, a, resumed, err)
}

func (m *Manager) startFile(j *job, t *task) *activeFile {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := &activeFile{name: filepath.Base(t.final), size: t.media.Size, base: -1}
	j.active[int64(t.msgID)] = a
	return a
}

func (m *Manager) fileProgress(a *activeFile, done int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if a.base < 0 {
		a.base = done // the first report is what an earlier attempt already left on disk
	}
	a.done = done
}

func (m *Manager) endFile(ctx context.Context, j *job, t *task, a *activeFile, resumed bool, err error) {
	m.mu.Lock()
	delete(j.active, int64(t.msgID))
	base := max(a.base, 0)
	m.mu.Unlock()
	if resumed {
		m.log.Info("resumed file", zap.String("job", j.id), zap.Int("msg", t.msgID), zap.Int64("from_bytes", base))
	}

	switch {
	case err == nil:
		mt := time.Unix(int64(t.date), 0)
		_ = os.Chtimes(t.final, mt, mt)
		m.mu.Lock()
		j.done++
		j.finished += t.media.Size - base
		m.mu.Unlock()
	case ctx.Err() != nil:
		// cancelled or shutting down: the partial file stays for the next attempt
	default:
		m.fileFailed(j, t.msgID, err.Error()+"（已下载的部分会保留，重试时接着下）")
	}
}
