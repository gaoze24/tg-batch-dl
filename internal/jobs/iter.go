package jobs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/gotd/td/tg"
	"github.com/iyear/tdl/core/tmedia"

	"github.com/gaoze24/tg-batch-dl/internal/fsname"
	"github.com/gaoze24/tg-batch-dl/internal/tgc"
)

const fetchBatch = 100

// task is one file to download.
type task struct {
	msgID int
	media *tmedia.Media
	final string
	date  int
}

// key identifies the remote file so saved partial progress is only reused for the very same file.
func (t *task) key() string {
	var id int64
	switch loc := t.media.InputFileLoc.(type) {
	case *tg.InputDocumentFileLocation:
		id = loc.ID
	case *tg.InputPhotoFileLocation:
		id = loc.ID
	}
	return strconv.FormatInt(id, 10) + ":" + strconv.FormatInt(t.media.Size, 10)
}

// lister walks the job's message ids, re-reading messages in batches right before download so file references are
// fresh. Per-message problems are recorded on the job and skipped; problems that stop the whole job go to fatal.
type lister struct {
	api  *tg.Client
	chat tgc.Chat
	ids  []int
	dir  string
	m    *Manager
	j    *job

	pos      int
	batch    map[int]*tg.Message
	batchEnd int
	fatal    error
}

func (l *lister) next(ctx context.Context) (*task, bool) {
	for l.pos < len(l.ids) {
		if ctx.Err() != nil {
			return nil, false
		}
		if l.pos >= l.batchEnd {
			end := min(l.pos+fetchBatch, len(l.ids))
			msgs, err := tgc.FetchMessages(ctx, l.api, l.chat.InputPeer(), l.ids[l.pos:end])
			if err != nil {
				if ctx.Err() == nil {
					l.fatal = fmt.Errorf("读取消息失败: %w", err)
				}
				return nil, false
			}
			l.batch, l.batchEnd = msgs, end
		}
		id := l.ids[l.pos]
		l.pos++

		msg, ok := l.batch[id]
		if !ok {
			l.m.fileFailed(l.j, id, "消息不存在或已被删除")
			continue
		}
		media, ok := tmedia.GetMedia(msg)
		if !ok {
			l.m.fileFailed(l.j, id, "这条消息没有可下载的文件")
			continue
		}
		final := filepath.Join(l.dir, fsname.File(msg.ID, media.Name, msg.Message, tgc.HasOwnFileName(msg)))
		if st, err := os.Stat(final); err == nil && st.Size() == media.Size {
			l.m.fileExisting(l.j)
			continue
		}
		return &task{msgID: msg.ID, media: media, final: final, date: msg.Date}, true
	}
	return nil, false
}

// remaining lists the ids never handed out.
func (l *lister) remaining() []int {
	if l.pos >= len(l.ids) {
		return nil
	}
	return l.ids[l.pos:]
}
