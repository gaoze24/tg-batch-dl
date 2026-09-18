// Package fsname builds file and folder names that are legal on Windows (the strictest target).
package fsname

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

var (
	illegal  = regexp.MustCompile(`[<>:"/\\|?*\x00-\x1f]`)
	spaces   = regexp.MustCompile(`\s+`)
	reserved = map[string]bool{"CON": true, "PRN": true, "AUX": true, "NUL": true}
)

func init() {
	for i := 1; i <= 9; i++ {
		reserved["COM"+strconv.Itoa(i)] = true
		reserved["LPT"+strconv.Itoa(i)] = true
	}
}

// Clean makes s safe as a single path element, cut to maxRunes; fallback is used when nothing is left.
func Clean(s string, maxRunes int, fallback string) string {
	s = illegal.ReplaceAllString(s, "_")
	s = strings.TrimSpace(spaces.ReplaceAllString(s, " "))
	if utf8.RuneCountInString(s) > maxRunes {
		s = string([]rune(s)[:maxRunes])
	}
	s = strings.TrimRight(s, ". ")
	if strings.Trim(s, "._ ") == "" {
		return fallback
	}
	if reserved[strings.ToUpper(strings.SplitN(s, ".", 2)[0])] {
		s = "_" + s
	}
	return s
}

// ChatDir is the per-chat folder name.
func ChatDir(title, ref string) string {
	return Clean(title, 60, ref)
}

// File names a downloaded file "<msgID>_<name>". When the uploader gave no file name (Telegram then only has
// "<docID>.mp4"), the first line of the caption is used instead, which is usually the video's title.
func File(msgID int, mediaName, caption string, ownName bool) string {
	ext := filepath.Ext(mediaName)
	if len(ext) > 10 {
		ext = ""
	}
	base := strings.TrimSuffix(mediaName, ext)
	if !ownName {
		if line := firstLine(caption); line != "" {
			base = line
		}
	}
	base = Clean(base, 80, "file")
	return strconv.Itoa(msgID) + "_" + base + Clean(ext, 10, "")
}

func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}
