package fsname

import (
	"strings"
	"testing"
)

func TestClean(t *testing.T) {
	cases := map[string]string{
		"My Channel":               "My Channel",
		`a<b>c:d"e/f\g|h?i*j`:      "a_b_c_d_e_f_g_h_i_j",
		"trailing dots... ":        "trailing dots",
		"CON":                      "_CON",
		"com1.backup":              "_com1.backup",
		"":                         "fb",
		"...":                      "fb",
		"//":                       "fb",
		"中文   频道":                  "中文 频道",
		"tab\tand\nnewline":        "tab_and_newline",
		"emoji 🎬 ok":               "emoji 🎬 ok",
		"LPT9":                     "_LPT9",
		"console":                  "console",
		"   spaced   out   name  ": "spaced out name",
	}
	for in, want := range cases {
		if got := Clean(in, 80, "fb"); got != want {
			t.Errorf("Clean(%q) = %q, want %q", in, got, want)
		}
	}
	if got := Clean(strings.Repeat("长", 200), 60, "fb"); len([]rune(got)) != 60 {
		t.Errorf("not truncated: %d runes", len([]rune(got)))
	}
}

func TestFile(t *testing.T) {
	cases := []struct {
		msgID   int
		name    string
		caption string
		own     bool
		want    string
	}{
		{12, "clip.mp4", "ignored caption", true, "12_clip.mp4"},
		{12, "5372.mp4", "第一集：开始\n更多说明", false, "12_第一集：开始.mp4"},
		{12, "5372.mp4", "", false, "12_5372.mp4"},
		{12, "5372.mp4", "\n\n  标题 / 带斜杠  \n", false, "12_标题 _ 带斜杠.mp4"},
		{7, "55.jpg", "", false, "7_55.jpg"},
		{7, "noext", "", true, "7_noext"},
		{7, "bad?.mp4", "", true, "7_bad_.mp4"},
	}
	for _, tc := range cases {
		if got := File(tc.msgID, tc.name, tc.caption, tc.own); got != tc.want {
			t.Errorf("File(%d, %q, %q, %v) = %q, want %q", tc.msgID, tc.name, tc.caption, tc.own, got, tc.want)
		}
	}
}
