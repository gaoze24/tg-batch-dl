package tgc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/tg"
	"github.com/iyear/tdl/core/dcpool"
	"github.com/iyear/tdl/core/tmedia"
)

// upload.getFile rules: offset divisible by 4 KB, limit divides 1 MB, and a request must not cross a 1 MB boundary.
// Always asking for whole 1 MB-aligned blocks satisfies all three.
const (
	streamChunk    = 1 << 20
	fileRefTTL     = 30 * time.Minute
	maxCachedFiles = 500
)

// MediaInfo describes a message's file for streaming.
type MediaInfo struct {
	Size int64
	Mime string
	Name string
}

type fileRef struct {
	loc  tg.InputFileLocationClass
	dc   int
	info MediaInfo
	at   time.Time
}

type fileCache struct {
	mu    sync.Mutex
	files map[string]fileRef
}

func (fc *fileCache) reset() {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	fc.files = nil
}

func (fc *fileCache) get(key string) (fileRef, bool) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	f, ok := fc.files[key]
	if !ok || time.Since(f.at) > fileRefTTL {
		return fileRef{}, false
	}
	return f, true
}

func (fc *fileCache) put(key string, f fileRef) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if fc.files == nil || len(fc.files) >= maxCachedFiles {
		fc.files = map[string]fileRef{}
	}
	fc.files[key] = f
}

// mediaFile resolves (and caches) the download location of a message's media; refresh forces a fresh file reference.
func (c *Client) mediaFile(ctx context.Context, ref string, id int, refresh bool) (fileRef, error) {
	key := thumbKey(ref, id)
	if !refresh {
		if f, ok := c.files.get(key); ok {
			return f, nil
		}
	}
	api, _, err := c.Session()
	if err != nil {
		return fileRef{}, err
	}
	chat, err := c.Peer(ctx, ref)
	if err != nil {
		return fileRef{}, err
	}
	msgs, err := FetchMessages(ctx, api, chat.peer, []int{id})
	if err != nil {
		return fileRef{}, err
	}
	msg, ok := msgs[id]
	if !ok {
		return fileRef{}, errNoMedia
	}
	media, ok := tmedia.GetMedia(msg)
	if !ok {
		return fileRef{}, errNoMedia
	}
	f := fileRef{loc: media.InputFileLoc, dc: media.DC, info: MediaInfo{Size: media.Size, Mime: mimeOf(msg), Name: media.Name}, at: time.Now()}
	c.files.put(key, f)
	return f, nil
}

func mimeOf(msg *tg.Message) string {
	if m, ok := msg.Media.(*tg.MessageMediaDocument); ok {
		if doc, ok := m.Document.(*tg.Document); ok && doc.MimeType != "" {
			return doc.MimeType
		}
		return "application/octet-stream"
	}
	return "image/jpeg" // Telegram photos are always JPEG
}

// OpenMedia returns size and type of a message's file.
func (c *Client) OpenMedia(ctx context.Context, ref string, id int) (MediaInfo, error) {
	f, err := c.mediaFile(ctx, ref, id, false)
	if err != nil {
		return MediaInfo{}, err
	}
	return f.info, nil
}

// ReadRange writes bytes [start, end] of a message's file to w, fetching 1 MB blocks from Telegram as it goes.
func (c *Client) ReadRange(ctx context.Context, ref string, id int, start, end int64, w io.Writer) error {
	f, err := c.mediaFile(ctx, ref, id, false)
	if err != nil {
		return err
	}
	_, pool, err := c.Session()
	if err != nil {
		return err
	}
	api := pool.Client(ctx, f.dc)
	for off := start - start%streamChunk; off <= end; off += streamChunk {
		b, err := getChunk(ctx, api, f.loc, off)
		if isFileRefErr(err) {
			if f, err = c.mediaFile(ctx, ref, id, true); err != nil {
				return err
			}
			b, err = getChunk(ctx, api, f.loc, off)
		}
		if err != nil {
			return err
		}
		lo, hi := max(0, start-off), min(int64(len(b)), end-off+1)
		if lo >= hi {
			return nil // past the end of the file
		}
		if _, err := w.Write(b[lo:hi]); err != nil {
			return err // viewer closed the player or seeked elsewhere
		}
		if len(b) < streamChunk {
			return nil
		}
	}
	return nil
}

// Fetcher reads 1 MB-aligned blocks of msgID's file from its data center, re-reading the message once to get a fresh
// file reference when Telegram says the old one expired. Safe for concurrent use.
func Fetcher(api *tg.Client, pool dcpool.Pool, peer tg.InputPeerClass, msgID int, media *tmedia.Media) func(ctx context.Context, offset int64) ([]byte, error) {
	var mu sync.Mutex
	loc := media.InputFileLoc
	return func(ctx context.Context, offset int64) ([]byte, error) {
		mu.Lock()
		cur := loc
		mu.Unlock()
		client := pool.Client(ctx, media.DC)
		b, err := getChunk(ctx, client, cur, offset)
		if !isFileRefErr(err) {
			return b, err
		}
		msgs, err := FetchMessages(ctx, api, peer, []int{msgID})
		if err != nil {
			return nil, err
		}
		msg, ok := msgs[msgID]
		if !ok {
			return nil, errNoMedia
		}
		fresh, ok := tmedia.GetMedia(msg)
		if !ok {
			return nil, errNoMedia
		}
		mu.Lock()
		loc = fresh.InputFileLoc
		mu.Unlock()
		return getChunk(ctx, client, fresh.InputFileLoc, offset)
	}
}

func getChunk(ctx context.Context, api *tg.Client, loc tg.InputFileLocationClass, offset int64) ([]byte, error) {
	res, err := api.UploadGetFile(ctx, &tg.UploadGetFileRequest{Location: loc, Offset: offset, Limit: streamChunk})
	if err != nil {
		return nil, err
	}
	file, ok := res.(*tg.UploadFile)
	if !ok {
		return nil, fmt.Errorf("unexpected upload.getFile result %T", res)
	}
	return file.Bytes, nil
}

// ErrBadRange means the Range header can't be satisfied.
var ErrBadRange = errors.New("bad range")

// ParseRange handles a single HTTP byte range ("bytes=a-b", "bytes=a-", "bytes=-n"). Without a header the whole file
// is returned with partial=false. Extra ranges after a comma are ignored.
func ParseRange(header string, size int64) (start, end int64, partial bool, err error) {
	if size <= 0 {
		return 0, -1, false, ErrBadRange
	}
	if header == "" {
		return 0, size - 1, false, nil
	}
	spec, ok := strings.CutPrefix(header, "bytes=")
	if !ok {
		return 0, 0, false, ErrBadRange
	}
	spec, _, _ = strings.Cut(spec, ",")
	a, b, ok := strings.Cut(strings.TrimSpace(spec), "-")
	if !ok {
		return 0, 0, false, ErrBadRange
	}
	switch {
	case a == "": // suffix: last b bytes
		n, err := strconv.ParseInt(b, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false, ErrBadRange
		}
		return max(0, size-n), size - 1, true, nil
	default:
		s, err := strconv.ParseInt(a, 10, 64)
		if err != nil || s < 0 || s >= size {
			return 0, 0, false, ErrBadRange
		}
		e := size - 1
		if b != "" {
			if e, err = strconv.ParseInt(b, 10, 64); err != nil || e < s {
				return 0, 0, false, ErrBadRange
			}
			e = min(e, size-1)
		}
		return s, e, true, nil
	}
}
