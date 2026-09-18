package tgc

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/gotd/td/tg"
)

const (
	dialogBatch = 100
	maxDialogs  = 5000
)

// Chat is one dialog. Ref is a stable string id: "c<id>" channel/supergroup, "g<id>" basic group, "u<id>" user.
type Chat struct {
	Ref      string `json:"ref"`
	Kind     string `json:"kind"` // channel | group | user | bot | self
	Title    string `json:"title"`
	Username string `json:"username,omitempty"`
	peer     tg.InputPeerClass
}

// InputPeer is the MTProto peer used for API calls.
func (c Chat) InputPeer() tg.InputPeerClass { return c.peer }

// ErrUnknownChat means the ref is not in the dialog list.
var ErrUnknownChat = errors.New("找不到这个聊天：可能没有加入，或者需要刷新聊天列表")

type chatCache struct {
	mu     sync.Mutex // also serialises loads
	loaded bool
	list   []Chat
	byRef  map[string]Chat
}

func (cc *chatCache) reset() {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	cc.loaded, cc.list, cc.byRef = false, nil, nil
}

func (cc *chatCache) add(ch Chat) {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	if cc.byRef == nil {
		cc.byRef = map[string]Chat{}
	}
	cc.byRef[ch.Ref] = ch
}

// ValidRef reports whether ref has the "c123" / "g123" / "u123" shape.
func ValidRef(ref string) bool {
	if len(ref) < 2 || !strings.ContainsRune("cgu", rune(ref[0])) {
		return false
	}
	id, err := strconv.ParseInt(ref[1:], 10, 64)
	return err == nil && id > 0
}

// Chats lists dialogs, cached after the first call unless refresh is set.
func (c *Client) Chats(ctx context.Context, refresh bool) ([]Chat, error) {
	api, _, err := c.Session()
	if err != nil {
		return nil, err
	}
	cc := &c.chats
	cc.mu.Lock()
	defer cc.mu.Unlock()
	if cc.loaded && !refresh {
		return append([]Chat(nil), cc.list...), nil
	}
	list, err := fetchDialogs(ctx, api)
	if err != nil {
		return nil, err
	}
	byRef := make(map[string]Chat, len(list))
	for ref, ch := range cc.byRef { // keep chats resolved from links
		byRef[ref] = ch
	}
	for _, ch := range list {
		byRef[ch.Ref] = ch
	}
	cc.loaded, cc.list, cc.byRef = true, list, byRef
	return append([]Chat(nil), list...), nil
}

// Peer looks a chat up by ref, loading the dialog list (once) if needed.
func (c *Client) Peer(ctx context.Context, ref string) (Chat, error) {
	if !ValidRef(ref) {
		return Chat{}, ErrUnknownChat
	}
	lookup := func() (Chat, bool, bool) {
		c.chats.mu.Lock()
		defer c.chats.mu.Unlock()
		ch, ok := c.chats.byRef[ref]
		return ch, ok, c.chats.loaded
	}
	if ch, ok, _ := lookup(); ok {
		return ch, nil
	}
	if _, err := c.Chats(ctx, true); err != nil {
		return Chat{}, err
	}
	if ch, ok, _ := lookup(); ok {
		return ch, nil
	}
	return Chat{}, ErrUnknownChat
}

type entities struct {
	users    map[int64]*tg.User
	chats    map[int64]*tg.Chat
	channels map[int64]*tg.Channel
}

func newEntities(users []tg.UserClass, chats []tg.ChatClass) entities {
	e := entities{users: map[int64]*tg.User{}, chats: map[int64]*tg.Chat{}, channels: map[int64]*tg.Channel{}}
	for _, u := range users {
		if user, ok := u.(*tg.User); ok {
			e.users[user.ID] = user
		}
	}
	for _, ch := range chats {
		switch v := ch.(type) {
		case *tg.Chat:
			e.chats[v.ID] = v
		case *tg.Channel:
			e.channels[v.ID] = v
		}
	}
	return e
}

// chatFromPeer builds a Chat for a dialog peer; false for peers we can't address (deleted, forbidden, deactivated).
func chatFromPeer(p tg.PeerClass, e entities) (Chat, bool) {
	switch v := p.(type) {
	case *tg.PeerUser:
		u, ok := e.users[v.UserID]
		if !ok {
			return Chat{}, false
		}
		ch := Chat{
			Ref:      "u" + strconv.FormatInt(u.ID, 10),
			Kind:     "user",
			Title:    strings.TrimSpace(u.FirstName + " " + u.LastName),
			Username: u.Username,
			peer:     &tg.InputPeerUser{UserID: u.ID, AccessHash: u.AccessHash},
		}
		switch {
		case u.Self:
			ch.Kind, ch.Title = "self", "收藏夹（Saved Messages）"
		case u.Bot:
			ch.Kind = "bot"
		}
		if ch.Title == "" {
			ch.Title = u.Username
		}
		if ch.Title == "" {
			ch.Title = "已注销的账号"
		}
		return ch, true
	case *tg.PeerChat:
		g, ok := e.chats[v.ChatID]
		if !ok || g.Deactivated {
			return Chat{}, false
		}
		return Chat{
			Ref:   "g" + strconv.FormatInt(g.ID, 10),
			Kind:  "group",
			Title: g.Title,
			peer:  &tg.InputPeerChat{ChatID: g.ID},
		}, true
	case *tg.PeerChannel:
		ch, ok := e.channels[v.ChannelID]
		if !ok {
			return Chat{}, false
		}
		kind := "group"
		if ch.Broadcast {
			kind = "channel"
		}
		return Chat{
			Ref:      "c" + strconv.FormatInt(ch.ID, 10),
			Kind:     kind,
			Title:    ch.Title,
			Username: ch.Username,
			peer:     &tg.InputPeerChannel{ChannelID: ch.ID, AccessHash: ch.AccessHash},
		}, true
	}
	return Chat{}, false
}

// fetchDialogs pages through messages.getDialogs, skipping dialogs whose peer can't be resolved instead of failing.
func fetchDialogs(ctx context.Context, api *tg.Client) ([]Chat, error) {
	var (
		out        []Chat
		seen       = map[string]bool{}
		offsetDate int
		offsetID   int
		offsetPeer tg.InputPeerClass = &tg.InputPeerEmpty{}
	)
	for len(out) < maxDialogs {
		res, err := api.MessagesGetDialogs(ctx, &tg.MessagesGetDialogsRequest{
			OffsetDate: offsetDate,
			OffsetID:   offsetID,
			OffsetPeer: offsetPeer,
			Limit:      dialogBatch,
		})
		if err != nil {
			return nil, fmt.Errorf("获取聊天列表失败: %w", err)
		}
		var (
			dialogs  []tg.DialogClass
			messages []tg.MessageClass
			users    []tg.UserClass
			chats    []tg.ChatClass
			complete bool
		)
		switch d := res.(type) {
		case *tg.MessagesDialogs:
			dialogs, messages, users, chats, complete = d.Dialogs, d.Messages, d.Users, d.Chats, true
		case *tg.MessagesDialogsSlice:
			dialogs, messages, users, chats = d.Dialogs, d.Messages, d.Users, d.Chats
		default:
			return out, nil
		}
		if len(dialogs) == 0 {
			return out, nil
		}

		ents := newEntities(users, chats)
		dates := map[int]int{}
		for _, m := range messages {
			if nm, ok := m.(tg.NotEmptyMessage); ok {
				dates[nm.GetID()] = nm.GetDate()
			}
		}
		var last *tg.Dialog
		var lastChat Chat
		for _, dc := range dialogs {
			d, ok := dc.(*tg.Dialog)
			if !ok {
				continue
			}
			ch, ok := chatFromPeer(d.Peer, ents)
			if !ok {
				continue
			}
			last, lastChat = d, ch
			if !seen[ch.Ref] {
				seen[ch.Ref] = true
				out = append(out, ch)
			}
		}
		if complete || len(dialogs) < dialogBatch || last == nil {
			return out, nil
		}
		nextDate, nextID := dates[last.TopMessage], last.TopMessage
		if nextDate == offsetDate && nextID == offsetID {
			return out, nil // no progress; avoid looping forever
		}
		offsetDate, offsetID, offsetPeer = nextDate, nextID, lastChat.peer
	}
	return out, nil
}
