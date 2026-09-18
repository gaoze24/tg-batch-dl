package jobs

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/iyear/tdl/core/downloader"
)

// progress receives tdl downloader callbacks for one job.
type progress struct {
	m *Manager
	j *job
}

func (p *progress) OnAdd(e downloader.Elem) {
	el := e.(*elem)
	p.m.mu.Lock()
	defer p.m.mu.Unlock()
	p.j.active[el.id] = &activeFile{name: filepath.Base(el.final), size: el.media.Size}
}

func (p *progress) OnDownload(e downloader.Elem, st downloader.ProgressState) {
	el := e.(*elem)
	p.m.mu.Lock()
	defer p.m.mu.Unlock()
	if a := p.j.active[el.id]; a != nil {
		a.done = st.Downloaded
	}
}

// OnDone finalises one file. tdl's downloader logs per-file errors and then reports success (err == nil), so
// completeness is judged by the byte count actually written, not by err.
func (p *progress) OnDone(e downloader.Elem, err error) {
	el := e.(*elem)
	closeErr := el.f.Close()

	p.m.mu.Lock()
	var written int64
	if a := p.j.active[el.id]; a != nil {
		written = a.done
	}
	delete(p.j.active, el.id)
	p.m.mu.Unlock()

	part := el.final + partExt
	switch {
	case err != nil:
		_ = os.Remove(part)
		if !errors.Is(err, context.Canceled) {
			p.m.fileFailed(p.j, el.msgID, err.Error())
		}
	case closeErr != nil:
		_ = os.Remove(part)
		p.m.fileFailed(p.j, el.msgID, "写入文件失败："+closeErr.Error())
	case written != el.media.Size:
		_ = os.Remove(part)
		p.m.fileFailed(p.j, el.msgID, "下载不完整（网络中断或被限流），可以重试")
	default:
		if rerr := os.Rename(part, el.final); rerr != nil {
			_ = os.Remove(part)
			p.m.fileFailed(p.j, el.msgID, "保存文件失败："+rerr.Error())
			return
		}
		_ = os.Chtimes(el.final, el.modTime(), el.modTime())
		p.m.mu.Lock()
		p.j.done++
		p.j.finished += el.media.Size
		p.m.mu.Unlock()
	}
}
