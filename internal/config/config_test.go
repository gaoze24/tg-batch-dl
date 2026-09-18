package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenCreatesDefaults(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Get(); got.Port != DefaultPort || got.Threads != DefaultThreads || got.Limit != DefaultLimit {
		t.Fatalf("unexpected defaults: %+v", got)
	}
	if _, err := os.Stat(filepath.Join(dir, fileName)); err != nil {
		t.Fatalf("config not written: %v", err)
	}
}

func TestUpdateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := st.Get()
	want.Threads = 8
	want.Proxy = "socks5://127.0.0.1:7890"
	want.DownloadDir = filepath.Join(dir, "dl")
	if _, err := st.Update(want); err != nil {
		t.Fatal(err)
	}
	again, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := again.Get(); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestValidate(t *testing.T) {
	abs := filepath.Join(t.TempDir(), "dl")
	cases := map[string]Settings{
		"low port":       {Port: 80, DownloadDir: abs, Threads: 4, Limit: 2},
		"relative dir":   {Port: DefaultPort, DownloadDir: "dl", Threads: 4, Limit: 2},
		"zero threads":   {Port: DefaultPort, DownloadDir: abs, Threads: 0, Limit: 2},
		"too many files": {Port: DefaultPort, DownloadDir: abs, Threads: 4, Limit: 20},
		"http proxy":     {Port: DefaultPort, DownloadDir: abs, Threads: 4, Limit: 2, Proxy: "http://127.0.0.1:8080"},
		"id no hash":     {Port: DefaultPort, DownloadDir: abs, Threads: 4, Limit: 2, APIID: 123},
		"bad hash":       {Port: DefaultPort, DownloadDir: abs, Threads: 4, Limit: 2, APIID: 123, APIHash: "xyz"},
	}
	for name, s := range cases {
		if err := s.Validate(); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	ok := Settings{
		Port: DefaultPort, DownloadDir: abs, Threads: 4, Limit: 2, Proxy: "socks5h://u:p@host:1080",
		APIID: 123, APIHash: "0123456789ABCDEF0123456789abcdef",
	}
	if err := ok.Validate(); err != nil {
		t.Errorf("valid settings rejected: %v", err)
	}
}
