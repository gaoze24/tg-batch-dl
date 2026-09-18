package tgc

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/gotd/td/telegram/downloader"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

const (
	maxThumbData = 800    // cached JPEGs (~10-20 KB each)
	maxThumbRefs = 50_000 // cached locations
	thumbWidth   = 320
	thumbWorkers = 4
)

type thumbRef struct {
	loc    tg.InputFileLocationClass
	dc     int
	inline []byte
}

type thumbCache struct {
	mu    sync.Mutex
	refs  map[string]thumbRef
	data  map[string][]byte
	order []string
	sem   chan struct{}
}

func newThumbCache() *thumbCache {
	return &thumbCache{refs: map[string]thumbRef{}, data: map[string][]byte{}, sem: make(chan struct{}, thumbWorkers)}
}

func thumbKey(ref string, id int) string { return fmt.Sprintf("%s/%d", ref, id) }

func (t *thumbCache) reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.refs, t.data, t.order = map[string]thumbRef{}, map[string][]byte{}, nil
}

func (t *thumbCache) remember(key string, r thumbRef) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.refs) >= maxThumbRefs {
		t.refs = map[string]thumbRef{}
	}
	t.refs[key] = r
}

func (t *thumbCache) lookup(key string) ([]byte, thumbRef, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	r, ok := t.refs[key]
	return t.data[key], r, ok
}

func (t *thumbCache) store(key string, b []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.data[key]; ok {
		return
	}
	if len(t.order) >= maxThumbData {
		delete(t.data, t.order[0])
		t.order = t.order[1:]
	}
	t.data[key] = b
	t.order = append(t.order, key)
}

// pickThumb chooses the size closest to thumbWidth px wide. Stripped ("i") sizes need client-side reconstruction and
// are skipped.
func pickThumb(sizes []tg.PhotoSizeClass) (typ string, inline []byte, ok bool) {
	best := -1
	for _, s := range sizes {
		var (
			t string
			w int
			b []byte
		)
		switch v := s.(type) {
		case *tg.PhotoSize:
			t, w = v.Type, v.W
		case *tg.PhotoSizeProgressive:
			t, w = v.Type, v.W
		case *tg.PhotoCachedSize:
			t, w, b = v.Type, v.W, v.Bytes
		default:
			continue
		}
		d := w - thumbWidth
		if d < 0 {
			d = -d
		}
		if best < 0 || d < best {
			best, typ, inline, ok = d, t, b, true
		}
	}
	return typ, inline, ok
}

func docThumb(doc *tg.Document) *thumbRef {
	typ, inline, ok := pickThumb(doc.Thumbs)
	if !ok {
		return nil
	}
	return &thumbRef{
		loc: &tg.InputDocumentFileLocation{
			ID:            doc.ID,
			AccessHash:    doc.AccessHash,
			FileReference: doc.FileReference,
			ThumbSize:     typ,
		},
		dc:     doc.DCID,
		inline: inline,
	}
}

func photoThumb(p *tg.Photo) *thumbRef {
	typ, inline, ok := pickThumb(p.Sizes)
	if !ok {
		return nil
	}
	return &thumbRef{
		loc: &tg.InputPhotoFileLocation{
			ID:            p.ID,
			AccessHash:    p.AccessHash,
			FileReference: p.FileReference,
			ThumbSize:     typ,
		},
		dc:     p.DCID,
		inline: inline,
	}
}

// Thumb returns a JPEG preview for a media message.
func (c *Client) Thumb(ctx context.Context, ref string, id int) ([]byte, error) {
	key := thumbKey(ref, id)
	data, tr, known := c.thumbs.lookup(key)
	if data != nil {
		return data, nil
	}
	var err error
	if !known {
		if tr, err = c.refreshThumb(ctx, ref, id); err != nil {
			return nil, err
		}
	}
	if tr.inline != nil {
		return tr.inline, nil
	}

	select {
	case c.thumbs.sem <- struct{}{}:
		defer func() { <-c.thumbs.sem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	data, err = c.downloadSmall(ctx, tr)
	if isFileRefErr(err) {
		if tr, err = c.refreshThumb(ctx, ref, id); err != nil {
			return nil, err
		}
		data, err = c.downloadSmall(ctx, tr)
	}
	if err != nil {
		return nil, err
	}
	c.thumbs.store(key, data)
	return data, nil
}

func (c *Client) refreshThumb(ctx context.Context, ref string, id int) (thumbRef, error) {
	api, _, err := c.Session()
	if err != nil {
		return thumbRef{}, err
	}
	chat, err := c.Peer(ctx, ref)
	if err != nil {
		return thumbRef{}, err
	}
	msgs, err := FetchMessages(ctx, api, chat.peer, []int{id})
	if err != nil {
		return thumbRef{}, err
	}
	msg, ok := msgs[id]
	if !ok {
		return thumbRef{}, errNoMedia
	}
	_, tr, ok := mediaItem(msg)
	if !ok || tr == nil {
		return thumbRef{}, errNoMedia
	}
	c.thumbs.remember(thumbKey(ref, id), *tr)
	return *tr, nil
}

func (c *Client) downloadSmall(ctx context.Context, tr thumbRef) ([]byte, error) {
	_, pool, err := c.Session()
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if _, err := downloader.NewDownloader().Download(pool.Client(ctx, tr.dc), tr.loc).Stream(ctx, &buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func isFileRefErr(err error) bool {
	e, ok := tgerr.As(err)
	return ok && strings.HasPrefix(e.Type, "FILE_REFERENCE_")
}
