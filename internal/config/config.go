// Package config stores user settings as JSON in the data directory.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

const (
	DefaultPort    = 17800
	DefaultThreads = 4
	DefaultLimit   = 2
	fileName       = "config.json"
)

// Settings are the user-editable values.
type Settings struct {
	Port        int    `json:"port"`         // local web UI port (restart to apply)
	DownloadDir string `json:"download_dir"` // root folder; each chat gets a sub-folder
	Threads     int    `json:"threads"`      // connections per file
	Limit       int    `json:"limit"`        // files downloaded at the same time
	Proxy       string `json:"proxy"`        // socks5://[user:pass@]host:port, empty = direct (restart to apply)
	// Optional own app credentials from my.telegram.org; empty = Telegram Desktop's public ones (restart to apply).
	APIID   int    `json:"api_id"`
	APIHash string `json:"api_hash"`
}

// Defaults returns settings for a fresh install.
func Defaults() Settings {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return Settings{
		Port:        DefaultPort,
		DownloadDir: filepath.Join(home, "Downloads", "tgdl"),
		Threads:     DefaultThreads,
		Limit:       DefaultLimit,
	}
}

// Validate normalises s in place and rejects values that can't work.
func (s *Settings) Validate() error {
	if s.Port < 1024 || s.Port > 65535 {
		return errors.New("端口必须在 1024-65535 之间")
	}
	if s.DownloadDir == "" {
		return errors.New("下载目录不能为空")
	}
	if !filepath.IsAbs(s.DownloadDir) {
		return errors.New("下载目录要写完整路径，例如 D:\\Telegram")
	}
	s.DownloadDir = filepath.Clean(s.DownloadDir)
	if s.Threads < 1 || s.Threads > 16 {
		return errors.New("单文件线程数要在 1-16 之间")
	}
	if s.Limit < 1 || s.Limit > 8 {
		return errors.New("同时下载数要在 1-8 之间")
	}
	if s.Proxy != "" {
		u, err := url.Parse(s.Proxy)
		if err != nil || (u.Scheme != "socks5" && u.Scheme != "socks5h") || u.Host == "" {
			return errors.New("代理只支持 socks5://主机:端口 的格式")
		}
	}
	s.APIHash = strings.ToLower(strings.TrimSpace(s.APIHash))
	if (s.APIID == 0) != (s.APIHash == "") {
		return errors.New("api_id 和 api_hash 要么都填，要么都留空")
	}
	if s.APIID < 0 || (s.APIHash != "" && !apiHashRE.MatchString(s.APIHash)) {
		return errors.New("api_id / api_hash 格式不对（api_hash 是 32 位十六进制）")
	}
	return nil
}

var apiHashRE = regexp.MustCompile(`^[0-9a-f]{32}$`)

// Store loads and saves Settings atomically.
type Store struct {
	path string
	mu   sync.RWMutex
	cur  Settings
}

// Open reads dir/config.json, creating it with defaults on first run.
func Open(dir string) (*Store, error) {
	st := &Store{path: filepath.Join(dir, fileName), cur: Defaults()}
	data, err := os.ReadFile(st.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return st, st.write(st.cur)
	case err != nil:
		return nil, err
	}
	s := Defaults()
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("%s 格式错误: %w", st.path, err)
	}
	if err := s.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", st.path, err)
	}
	st.cur = s
	return st, nil
}

// Get returns a copy of the current settings.
func (st *Store) Get() Settings {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return st.cur
}

// Update validates and persists new settings.
func (st *Store) Update(s Settings) (Settings, error) {
	if err := s.Validate(); err != nil {
		return Settings{}, err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := st.write(s); err != nil {
		return Settings{}, err
	}
	st.cur = s
	return s, nil
}

func (st *Store) write(s Settings) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := st.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, st.path)
}
