package tgc

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/gotd/td/tg"
)

// Link is a parsed t.me message link.
type Link struct {
	ChannelID int64  // from t.me/c/<id>/<msg>
	Username  string // from t.me/<username>/<msg>
	MsgID     int
}

var usernameRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{3,31}$`)

// ParseLink accepts t.me/c/<id>/<msg>, t.me/c/<id>/<topic>/<msg>, t.me/<user>/<msg> and t.me/<user>/<topic>/<msg>.
func ParseLink(raw string) (Link, error) {
	s := strings.TrimSpace(raw)
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return Link{}, fmt.Errorf("链接格式不对: %s", raw)
	}
	switch strings.ToLower(u.Hostname()) {
	case "t.me", "www.t.me", "telegram.me", "www.telegram.me":
	default:
		return Link{}, fmt.Errorf("不是 t.me 消息链接: %s", raw)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return Link{}, fmt.Errorf("不是 t.me 消息链接: %s", raw)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	msgID, err := strconv.Atoi(parts[len(parts)-1])
	if err != nil || msgID <= 0 {
		return Link{}, fmt.Errorf("链接里没有消息编号: %s", raw)
	}
	if parts[0] == "c" {
		if len(parts) != 3 && len(parts) != 4 {
			return Link{}, fmt.Errorf("链接格式不对: %s", raw)
		}
		id, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || id <= 0 {
			return Link{}, fmt.Errorf("链接格式不对: %s", raw)
		}
		return Link{ChannelID: id, MsgID: msgID}, nil
	}
	if (len(parts) != 2 && len(parts) != 3) || !usernameRE.MatchString(parts[0]) {
		return Link{}, fmt.Errorf("链接格式不对: %s", raw)
	}
	return Link{Username: parts[0], MsgID: msgID}, nil
}

// ResolveLink finds the chat a link points to (joined chats only for t.me/c links).
func (c *Client) ResolveLink(ctx context.Context, l Link) (Chat, error) {
	if l.ChannelID != 0 {
		ch, err := c.Peer(ctx, "c"+strconv.FormatInt(l.ChannelID, 10))
		if errors.Is(err, ErrUnknownChat) {
			return Chat{}, fmt.Errorf("没找到频道 %d：私有频道的链接只能下载你已加入的", l.ChannelID)
		}
		return ch, err
	}
	api, _, err := c.Session()
	if err != nil {
		return Chat{}, err
	}
	res, err := api.ContactsResolveUsername(ctx, &tg.ContactsResolveUsernameRequest{Username: l.Username})
	if err != nil {
		return Chat{}, fmt.Errorf("找不到 @%s: %w", l.Username, err)
	}
	ch, ok := chatFromPeer(res.Peer, newEntities(res.Users, res.Chats))
	if !ok {
		return Chat{}, fmt.Errorf("无法访问 @%s", l.Username)
	}
	c.chats.add(ch)
	return ch, nil
}
