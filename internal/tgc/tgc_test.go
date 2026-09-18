package tgc

import (
	"testing"

	"github.com/gotd/td/tg"
)

func TestParseLink(t *testing.T) {
	ok := map[string]Link{
		"https://t.me/c/1697797156/151":            {ChannelID: 1697797156, MsgID: 151},
		"https://t.me/c/1492447836/251011/269724":  {ChannelID: 1492447836, MsgID: 269724},
		"t.me/telegram/193":                        {Username: "telegram", MsgID: 193},
		"https://t.me/somechannel/12?single":       {Username: "somechannel", MsgID: 12},
		"http://telegram.me/somechannel/7/12":      {Username: "somechannel", MsgID: 12},
		"  https://t.me/c/1697797156/151/  ":       {ChannelID: 1697797156, MsgID: 151},
		"https://T.ME/telegram/193":                {Username: "telegram", MsgID: 193},
		"https://www.t.me/c/1697797156/151?thread": {ChannelID: 1697797156, MsgID: 151},
	}
	for in, want := range ok {
		got, err := ParseLink(in)
		if err != nil || got != want {
			t.Errorf("ParseLink(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	bad := []string{
		"https://t.me/c/abc/1",
		"https://t.me/somechannel",
		"https://evil.com/c/1/2",
		"https://t.me/c/1/2/3/4",
		"ftp://t.me/c/1/2",
		"https://t.me/ab/2",
		"https://t.me/c/1/0",
		"",
	}
	for _, in := range bad {
		if got, err := ParseLink(in); err == nil {
			t.Errorf("ParseLink(%q) = %+v, want error", in, got)
		}
	}
}

func TestValidRef(t *testing.T) {
	for _, ref := range []string{"c1697797156", "g42", "u777000"} {
		if !ValidRef(ref) {
			t.Errorf("%q should be valid", ref)
		}
	}
	for _, ref := range []string{"", "c", "x12", "c-5", "c0", "u12a", "c12 "} {
		if ValidRef(ref) {
			t.Errorf("%q should be invalid", ref)
		}
	}
}

func TestChatFromPeer(t *testing.T) {
	ents := newEntities(
		[]tg.UserClass{
			&tg.User{ID: 1, AccessHash: 11, FirstName: "Ann", LastName: "Lee", Username: "ann"},
			&tg.User{ID: 2, AccessHash: 22, Bot: true, FirstName: "Helper"},
			&tg.User{ID: 3, Self: true, FirstName: "Me"},
		},
		[]tg.ChatClass{
			&tg.Channel{ID: 100, AccessHash: 1000, Title: "News", Broadcast: true, Username: "news"},
			&tg.Channel{ID: 101, AccessHash: 1010, Title: "Chat", Megagroup: true},
			&tg.Chat{ID: 5, Title: "Old group"},
			&tg.Chat{ID: 6, Title: "Dead", Deactivated: true},
		},
	)
	cases := []struct {
		peer tg.PeerClass
		ref  string
		kind string
		ok   bool
	}{
		{&tg.PeerUser{UserID: 1}, "u1", "user", true},
		{&tg.PeerUser{UserID: 2}, "u2", "bot", true},
		{&tg.PeerUser{UserID: 3}, "u3", "self", true},
		{&tg.PeerUser{UserID: 9}, "", "", false},
		{&tg.PeerChannel{ChannelID: 100}, "c100", "channel", true},
		{&tg.PeerChannel{ChannelID: 101}, "c101", "group", true},
		{&tg.PeerChat{ChatID: 5}, "g5", "group", true},
		{&tg.PeerChat{ChatID: 6}, "", "", false},
	}
	for _, tc := range cases {
		ch, ok := chatFromPeer(tc.peer, ents)
		if ok != tc.ok || ch.Ref != tc.ref || ch.Kind != tc.kind {
			t.Errorf("%T: got %+v ok=%v", tc.peer, ch, ok)
		}
	}
	ch, _ := chatFromPeer(&tg.PeerChannel{ChannelID: 100}, ents)
	if p, ok := ch.InputPeer().(*tg.InputPeerChannel); !ok || p.AccessHash != 1000 {
		t.Errorf("channel input peer = %#v", ch.InputPeer())
	}
}

func TestMediaItemVideo(t *testing.T) {
	msg := &tg.Message{
		ID:      42,
		Date:    1700000000,
		Message: "a caption",
		Media: &tg.MessageMediaDocument{Document: &tg.Document{
			ID: 9, AccessHash: 8, DCID: 4, Size: 123456, MimeType: "video/mp4",
			Thumbs: []tg.PhotoSizeClass{
				&tg.PhotoStrippedSize{Type: "i", Bytes: []byte{1, 2}},
				&tg.PhotoSize{Type: "m", W: 320, H: 180, Size: 9000},
			},
			Attributes: []tg.DocumentAttributeClass{
				&tg.DocumentAttributeVideo{Duration: 61.5, W: 1280, H: 720},
				&tg.DocumentAttributeFilename{FileName: "clip.mp4"},
			},
		}},
	}
	item, thumb, ok := mediaItem(msg)
	if !ok {
		t.Fatal("expected media")
	}
	if item.Kind != "video" || item.Name != "clip.mp4" || item.Size != 123456 || item.Duration != 61.5 || item.Width != 1280 {
		t.Errorf("unexpected item %+v", item)
	}
	if thumb == nil || thumb.dc != 4 {
		t.Fatalf("unexpected thumb %+v", thumb)
	}
	if loc, ok := thumb.loc.(*tg.InputDocumentFileLocation); !ok || loc.ThumbSize != "m" {
		t.Errorf("thumb should use size m, got %#v", thumb.loc)
	}
	if !HasOwnFileName(msg) {
		t.Error("HasOwnFileName should be true")
	}
}

func TestMediaItemPhotoAndText(t *testing.T) {
	photo := &tg.Message{ID: 7, Media: &tg.MessageMediaPhoto{Photo: &tg.Photo{
		ID: 55, DCID: 2,
		Sizes: []tg.PhotoSizeClass{
			&tg.PhotoSize{Type: "s", W: 90, H: 90, Size: 1000},
			&tg.PhotoSize{Type: "m", W: 320, H: 320, Size: 5000},
			&tg.PhotoSizeProgressive{Type: "y", W: 1280, H: 1280, Sizes: []int{10, 20, 90000}},
		},
	}}}
	item, thumb, ok := mediaItem(photo)
	if !ok || item.Kind != "photo" || item.Size != 90000 || item.Width != 1280 || item.Name != "55.jpg" {
		t.Fatalf("unexpected photo item %+v ok=%v", item, ok)
	}
	if loc, ok := thumb.loc.(*tg.InputPhotoFileLocation); !ok || loc.ThumbSize != "m" {
		t.Errorf("photo thumb should use size m, got %#v", thumb.loc)
	}
	if HasOwnFileName(photo) {
		t.Error("photos have no own file name")
	}
	if _, _, ok := mediaItem(&tg.Message{ID: 8, Message: "just text"}); ok {
		t.Error("text message should not be media")
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("你好世界", 2); got != "你好…" {
		t.Errorf("got %q", got)
	}
	if got := truncate("short", 10); got != "short" {
		t.Errorf("got %q", got)
	}
}
