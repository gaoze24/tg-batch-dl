package jobs

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/gotd/td/tg"
	"github.com/iyear/tdl/core/downloader"
	"github.com/iyear/tdl/core/tmedia"
	"go.uber.org/zap/zapcore"

	"github.com/gaoze24/tg-batch-dl/internal/fsname"
	"github.com/gaoze24/tg-batch-dl/internal/tgc"
)

const (
	partExt    = ".part"
	fetchBatch = 100
)

// iter feeds tdl's downloader. Messages are re-fetched in batches right before download so file references are fresh.
//
// Per-message problems are recorded on the job and skipped. Err() always returns nil: tdl's Download returns early on
// an iterator error without waiting for in-flight files, so fatal problems go to it.fatal and simply stop iteration.
type iter struct {
	api  *tg.Client
	chat tgc.Chat
	ids  []int
	dir  string
	m    *Manager
	j    *job

	pos      int
	batch    map[int]*tg.Message
	batchEnd int
	seq      int64
	cur      *elem
	fatal    error
}

func (it *iter) Next(ctx context.Context) bool {
	for it.pos < len(it.ids) {
		if ctx.Err() != nil {
			return false
		}
		if it.pos >= it.batchEnd {
			end := min(it.pos+fetchBatch, len(it.ids))
			msgs, err := tgc.FetchMessages(ctx, it.api, it.chat.InputPeer(), it.ids[it.pos:end])
			if err != nil {
				if ctx.Err() == nil {
					it.fatal = fmt.Errorf("读取消息失败: %w", err)
				}
				return false
			}
			it.batch, it.batchEnd = msgs, end
		}
		id := it.ids[it.pos]
		it.pos++

		msg, ok := it.batch[id]
		if !ok {
			it.m.fileFailed(it.j, id, "消息不存在或已被删除")
			continue
		}
		media, ok := tmedia.GetMedia(msg)
		if !ok {
			it.m.fileFailed(it.j, id, "这条消息没有可下载的文件")
			continue
		}
		final := filepath.Join(it.dir, fsname.File(msg.ID, media.Name, msg.Message, tgc.HasOwnFileName(msg)))
		if st, err := os.Stat(final); err == nil && st.Size() == media.Size {
			it.m.fileExisting(it.j)
			continue
		}
		f, err := os.Create(final + partExt)
		if err != nil {
			it.fatal = fmt.Errorf("无法创建文件: %w", err)
			return false
		}
		it.seq++
		it.cur = &elem{id: it.seq, msgID: msg.ID, media: media, f: f, final: final, date: msg.Date}
		return true
	}
	return false
}

func (it *iter) Value() downloader.Elem { return it.cur }
func (it *iter) Err() error             { return nil }

// remaining lists the ids the downloader never asked for.
func (it *iter) remaining() []int {
	if it.pos >= len(it.ids) {
		return nil
	}
	return it.ids[it.pos:]
}

type elem struct {
	id    int64
	msgID int
	media *tmedia.Media
	f     *os.File
	final string
	date  int
}

func (e *elem) File() downloader.File { return fileInfo{e.media} }
func (e *elem) To() io.WriterAt       { return e.f }
func (e *elem) AsTakeout() bool       { return false }

// MarshalLogObject keeps tdl's error logs to the message id instead of dumping the whole struct.
func (e *elem) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	enc.AddInt("msg", e.msgID)
	return nil
}

func (e *elem) modTime() time.Time { return time.Unix(int64(e.date), 0) }

type fileInfo struct{ m *tmedia.Media }

func (f fileInfo) Location() tg.InputFileLocationClass { return f.m.InputFileLoc }
func (f fileInfo) Size() int64                         { return f.m.Size }
func (f fileInfo) DC() int                             { return f.m.DC }
