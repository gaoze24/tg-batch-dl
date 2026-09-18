package tgc

import (
	"errors"
	"testing"
)

func TestParseRange(t *testing.T) {
	const size = 1000
	cases := []struct {
		header       string
		start, end   int64
		partial, bad bool
	}{
		{"", 0, 999, false, false},
		{"bytes=0-", 0, 999, true, false},
		{"bytes=100-199", 100, 199, true, false},
		{"bytes=900-5000", 900, 999, true, false}, // end clamped
		{"bytes=-100", 900, 999, true, false},     // last 100 bytes
		{"bytes=-5000", 0, 999, true, false},
		{"bytes=0-9, 20-29", 0, 9, true, false}, // extra ranges ignored
		{"bytes=1000-", 0, 0, false, true},     // starts past the end
		{"bytes=200-100", 0, 0, false, true},
		{"bytes=abc-", 0, 0, false, true},
		{"items=0-1", 0, 0, false, true},
		{"bytes=-0", 0, 0, false, true},
	}
	for _, tc := range cases {
		s, e, partial, err := ParseRange(tc.header, size)
		if tc.bad {
			if !errors.Is(err, ErrBadRange) {
				t.Errorf("%q: expected ErrBadRange, got %d-%d %v", tc.header, s, e, err)
			}
			continue
		}
		if err != nil || s != tc.start || e != tc.end || partial != tc.partial {
			t.Errorf("%q: got %d-%d partial=%v err=%v", tc.header, s, e, partial, err)
		}
	}
	if _, _, _, err := ParseRange("", 0); !errors.Is(err, ErrBadRange) {
		t.Error("empty file should not be servable")
	}
}

func TestRangeMatch(t *testing.T) {
	video := MediaItem{Kind: "video", Size: 50 << 20, Duration: 600}
	photo := MediaItem{Kind: "photo", Size: 2 << 20}
	cases := []struct {
		r     Range
		video bool
		photo bool
	}{
		{Range{}, true, true},
		{Range{MinSize: 10 << 20}, true, false},
		{Range{MaxSize: 10 << 20}, false, true},
		{Range{MinDuration: 300}, true, true}, // duration bounds ignore non-videos
		{Range{MaxDuration: 300}, false, true},
		{Range{MinSize: 1 << 20, MaxSize: 100 << 20, MinDuration: 60, MaxDuration: 900}, true, true},
	}
	for _, tc := range cases {
		if got := tc.r.Match(video); got != tc.video {
			t.Errorf("%+v video: got %v", tc.r, got)
		}
		if got := tc.r.Match(photo); got != tc.photo {
			t.Errorf("%+v photo: got %v", tc.r, got)
		}
	}
}
