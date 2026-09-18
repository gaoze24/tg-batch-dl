package tgc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/gotd/td/tg"
	"github.com/iyear/tdl/core/tmedia"
	"github.com/iyear/tdl/core/util/tutil"
)

const (
	searchBatch   = 100
	maxListedIDs  = 200_000
	maxCaptionLen = 300
)

// MediaItem is one downloadable message as shown in the grid.
type MediaItem struct {
	ID        int     `json:"id"`
	Date      int     `json:"date"`
	Caption   string  `json:"caption,omitempty"`
	Name      string  `json:"name"`
	Size      int64   `json:"size"`
	Kind      string  `json:"kind"` // video | photo | file
	Mime      string  `json:"mime,omitempty"`
	Duration  float64 `json:"duration,omitempty"`
	Width     int     `json:"width,omitempty"`
	Height    int     `json:"height,omitempty"`
	Thumb     bool    `json:"thumb"`
	GroupedID int64   `json:"grouped_id,omitempty"`
}

type MediaPage struct {
	Items      []MediaItem `json:"items"`
	NextOffset int         `json:"next_offset"` // 0 = no more
	Total      int         `json:"total"`
}

var filters = map[string]func() tg.MessagesFilterClass{
	"video": func() tg.MessagesFilterClass { return &tg.InputMessagesFilterVideo{} },
	"photo": func() tg.MessagesFilterClass { return &tg.InputMessagesFilterPhotos{} },
	"media": func() tg.MessagesFilterClass { return &tg.InputMessagesFilterPhotoVideo{} },
	"file":  func() tg.MessagesFilterClass { return &tg.InputMessagesFilterDocument{} },
}

// ValidFilter reports whether name is a supported media filter.
func ValidFilter(name string) bool {
	_, ok := filters[name]
	return ok
}

func unpackMessages(res tg.MessagesMessagesClass) ([]tg.MessageClass, int) {
	switch m := res.(type) {
	case *tg.MessagesMessages:
		return m.Messages, len(m.Messages)
	case *tg.MessagesMessagesSlice:
		return m.Messages, m.Count
	case *tg.MessagesChannelMessages:
		return m.Messages, m.Count
	}
	return nil, 0
}

func search(ctx context.Context, api *tg.Client, peer tg.InputPeerClass, filter string, offsetID, limit int) ([]tg.MessageClass, int, error) {
	mk, ok := filters[filter]
	if !ok {
		return nil, 0, fmt.Errorf("unknown filter %q", filter)
	}
	res, err := api.MessagesSearch(ctx, &tg.MessagesSearchRequest{
		Peer:     peer,
		Filter:   mk(),
		OffsetID: offsetID,
		Limit:    limit,
	})
	if err != nil {
		return nil, 0, err
	}
	msgs, total := unpackMessages(res)
	return msgs, total, nil
}

// Media returns one page of media messages, newest first. Pass NextOffset back to get the following page.
func (c *Client) Media(ctx context.Context, ref, filter string, offsetID, limit int) (MediaPage, error) {
	api, _, err := c.Session()
	if err != nil {
		return MediaPage{}, err
	}
	chat, err := c.Peer(ctx, ref)
	if err != nil {
		return MediaPage{}, err
	}
	msgs, total, err := search(ctx, api, chat.peer, filter, offsetID, limit)
	if err != nil {
		return MediaPage{}, err
	}
	page := MediaPage{Items: []MediaItem{}, Total: total}
	for _, m := range msgs {
		msg, ok := m.(*tg.Message)
		if !ok {
			continue
		}
		item, thumb, ok := mediaItem(msg)
		if !ok {
			continue
		}
		if thumb != nil {
			c.thumbs.remember(thumbKey(ref, msg.ID), *thumb)
		}
		page.Items = append(page.Items, item)
	}
	if len(msgs) >= limit {
		if last, ok := msgs[len(msgs)-1].(tg.NotEmptyMessage); ok {
			page.NextOffset = last.GetID()
		}
	}
	return page, nil
}

// ListMediaIDs collects every media message id in the chat matching filter, oldest first.
func (c *Client) ListMediaIDs(ctx context.Context, chat Chat, filter string, progress func(n int)) ([]int, error) {
	api, _, err := c.Session()
	if err != nil {
		return nil, err
	}
	var ids []int
	offset := 0
	for len(ids) < maxListedIDs {
		msgs, _, err := search(ctx, api, chat.peer, filter, offset, searchBatch)
		if err != nil {
			return nil, err
		}
		for _, m := range msgs {
			if msg, ok := m.(*tg.Message); ok {
				if _, ok := tmedia.GetMedia(msg); ok {
					ids = append(ids, msg.ID)
				}
			}
		}
		if progress != nil {
			progress(len(ids))
		}
		if len(msgs) < searchBatch {
			break
		}
		last, ok := msgs[len(msgs)-1].(tg.NotEmptyMessage)
		if !ok || last.GetID() == offset {
			break
		}
		offset = last.GetID()
	}
	for i, j := 0, len(ids)-1; i < j; i, j = i+1, j-1 {
		ids[i], ids[j] = ids[j], ids[i]
	}
	return ids, nil
}

// FetchMessages re-reads messages by id, which also refreshes their file references. Missing ids are simply absent.
func FetchMessages(ctx context.Context, api *tg.Client, peer tg.InputPeerClass, ids []int) (map[int]*tg.Message, error) {
	out := make(map[int]*tg.Message, len(ids))
	for start := 0; start < len(ids); start += searchBatch {
		chunk := ids[start:min(start+searchBatch, len(ids))]
		input := make([]tg.InputMessageClass, len(chunk))
		for i, id := range chunk {
			input[i] = &tg.InputMessageID{ID: id}
		}
		var (
			res tg.MessagesMessagesClass
			err error
		)
		channel, isChannel := peer.(*tg.InputPeerChannel)
		if isChannel {
			res, err = api.ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{
				Channel: &tg.InputChannel{ChannelID: channel.ChannelID, AccessHash: channel.AccessHash},
				ID:      input,
			})
		} else {
			res, err = api.MessagesGetMessages(ctx, input)
		}
		if err != nil {
			return nil, err
		}
		msgs, _ := unpackMessages(res)
		want := tutil.GetInputPeerID(peer)
		for _, m := range msgs {
			msg, ok := m.(*tg.Message)
			if !ok {
				continue
			}
			// messages.getMessages works on the account-wide id space; make sure it belongs to this chat
			if !isChannel && tutil.GetPeerID(msg.PeerID) != want {
				continue
			}
			out[msg.ID] = msg
		}
	}
	return out, nil
}

// mediaItem describes msg for the grid and picks a thumbnail; false if it has nothing downloadable.
func mediaItem(msg *tg.Message) (MediaItem, *thumbRef, bool) {
	media, ok := msg.GetMedia()
	if !ok {
		return MediaItem{}, nil, false
	}
	item := MediaItem{ID: msg.ID, Date: msg.Date, Caption: truncate(msg.Message, maxCaptionLen)}
	if g, ok := msg.GetGroupedID(); ok {
		item.GroupedID = g
	}
	switch m := media.(type) {
	case *tg.MessageMediaDocument:
		doc, ok := m.Document.(*tg.Document)
		if !ok {
			return MediaItem{}, nil, false
		}
		item.Name = tmedia.GetDocumentName(doc)
		item.Size = doc.Size
		item.Mime = doc.MimeType
		item.Kind = "file"
		for _, a := range doc.Attributes {
			if v, ok := a.(*tg.DocumentAttributeVideo); ok {
				item.Kind, item.Duration, item.Width, item.Height = "video", v.Duration, v.W, v.H
			}
		}
		if item.Kind == "file" && strings.HasPrefix(doc.MimeType, "video/") {
			item.Kind = "video"
		}
		thumb := docThumb(doc)
		item.Thumb = thumb != nil
		return item, thumb, true
	case *tg.MessageMediaPhoto:
		photo, ok := m.Photo.(*tg.Photo)
		if !ok || len(photo.Sizes) == 0 {
			return MediaItem{}, nil, false
		}
		info, ok := tmedia.GetPhotoInfo(m)
		if !ok {
			return MediaItem{}, nil, false
		}
		item.Name, item.Size, item.Kind = info.Name, info.Size, "photo"
		item.Width, item.Height = largestDims(photo.Sizes)
		thumb := photoThumb(photo)
		item.Thumb = thumb != nil
		return item, thumb, true
	}
	return MediaItem{}, nil, false
}

// HasOwnFileName reports whether the document carries an uploader-provided file name
// (otherwise tdl generates "<id>.<ext>" and a caption makes a better name).
func HasOwnFileName(msg *tg.Message) bool {
	m, ok := msg.Media.(*tg.MessageMediaDocument)
	if !ok {
		return false
	}
	doc, ok := m.Document.(*tg.Document)
	if !ok {
		return false
	}
	for _, a := range doc.Attributes {
		if _, ok := a.(*tg.DocumentAttributeFilename); ok {
			return true
		}
	}
	return false
}

func largestDims(sizes []tg.PhotoSizeClass) (int, int) {
	w, h := 0, 0
	for _, s := range sizes {
		switch v := s.(type) {
		case *tg.PhotoSize:
			if v.W > w {
				w, h = v.W, v.H
			}
		case *tg.PhotoSizeProgressive:
			if v.W > w {
				w, h = v.W, v.H
			}
		}
	}
	return w, h
}

func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}

var errNoMedia = errors.New("这条消息没有可下载的文件")
