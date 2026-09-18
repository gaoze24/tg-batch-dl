package jobs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// fakeRemote serves a byte slice in partSize blocks, optionally dropping the connection after a number of calls.
type fakeRemote struct {
	data      []byte
	failAfter int // fail every call after this many; <0 = never
	shortOnce map[int64]bool

	mu    sync.Mutex
	n     int
	calls map[int64]int
}

func newRemote(size int64, failAfter int) *fakeRemote {
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i*7 + i/partSize)
	}
	return &fakeRemote{data: data, failAfter: failAfter, calls: map[int64]int{}, shortOnce: map[int64]bool{}}
}

func (r *fakeRemote) fetch(ctx context.Context, off int64) ([]byte, error) {
	r.mu.Lock()
	r.n++
	r.calls[off]++
	n, short := r.n, r.shortOnce[off]
	delete(r.shortOnce, off)
	r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.failAfter >= 0 && n > r.failAfter {
		return nil, errors.New("connection dropped")
	}
	end := min(off+partSize, int64(len(r.data)))
	b := append([]byte(nil), r.data[off:end]...)
	if short {
		b = b[:len(b)/2]
	}
	return b, nil
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestDownloadFresh(t *testing.T) {
	final := filepath.Join(t.TempDir(), "v.mp4")
	size := int64(3*partSize + 12345)
	r := newRemote(size, -1)
	var last int64
	resumed, err := downloadFile(context.Background(), final, size, "k", 3, r.fetch, func(d int64) { last = d })
	if err != nil || resumed {
		t.Fatalf("err=%v resumed=%v", err, resumed)
	}
	got, _ := os.ReadFile(final)
	if !bytes.Equal(got, r.data) {
		t.Fatal("content mismatch")
	}
	if last != size || exists(final+partExt) || exists(final+stateExt) {
		t.Errorf("last=%d part=%v state=%v", last, exists(final+partExt), exists(final+stateExt))
	}
}

func TestDownloadResumesAfterDrop(t *testing.T) {
	final := filepath.Join(t.TempDir(), "v.mp4")
	size := int64(4*partSize + 99)
	// one connection so blocks go in order: 0 and 1 succeed, then the connection drops
	first := newRemote(size, 2)
	if _, err := downloadFile(context.Background(), final, size, "k", 1, first.fetch, func(int64) {}); err == nil {
		t.Fatal("expected an error from the dropped connection")
	}
	if !exists(final+partExt) || !exists(final+stateExt) || exists(final) {
		t.Fatal("partial file and state should be kept, final not published")
	}

	second := newRemote(size, -1)
	var firstReport int64 = -1
	resumed, err := downloadFile(context.Background(), final, size, "k", 2, second.fetch, func(d int64) {
		if firstReport < 0 {
			firstReport = d
		}
	})
	if err != nil || !resumed {
		t.Fatalf("resume: err=%v resumed=%v", err, resumed)
	}
	if firstReport != 2*partSize {
		t.Errorf("resumed from %d bytes, want %d", firstReport, 2*partSize)
	}
	if second.calls[0] != 0 || second.calls[partSize] != 0 {
		t.Errorf("already downloaded blocks were fetched again: %v", second.calls)
	}
	got, _ := os.ReadFile(final)
	if !bytes.Equal(got, second.data) {
		t.Fatal("content mismatch after resume")
	}
	if exists(final+partExt) || exists(final+stateExt) {
		t.Error("leftover part/state after completion")
	}
}

func TestDownloadDifferentFileStartsOver(t *testing.T) {
	final := filepath.Join(t.TempDir(), "v.mp4")
	size := int64(3 * partSize)
	if _, err := downloadFile(context.Background(), final, size, "old", 1, newRemote(size, 1).fetch, func(int64) {}); err == nil {
		t.Fatal("expected failure")
	}
	r := newRemote(size, -1)
	resumed, err := downloadFile(context.Background(), final, size, "new", 1, r.fetch, func(int64) {})
	if err != nil || resumed || r.calls[0] != 1 {
		t.Errorf("err=%v resumed=%v calls=%v", err, resumed, r.calls)
	}
}

func TestDownloadCancelledKeepsProgress(t *testing.T) {
	final := filepath.Join(t.TempDir(), "v.mp4")
	size := int64(3 * partSize)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := downloadFile(ctx, final, size, "k", 2, newRemote(size, -1).fetch, func(int64) {})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if exists(final) {
		t.Error("cancelled download must not be published")
	}
}

func TestDownloadShortBlocks(t *testing.T) {
	final := filepath.Join(t.TempDir(), "v.mp4")
	size := int64(2*partSize + 10)
	r := newRemote(size, -1)
	r.shortOnce[partSize] = true // one short answer is retried
	if _, err := downloadFile(context.Background(), final, size, "k", 1, r.fetch, func(int64) {}); err != nil {
		t.Fatalf("short block should be retried: %v", err)
	}
	if got, _ := os.ReadFile(final); !bytes.Equal(got, r.data) {
		t.Fatal("content mismatch")
	}

	final2 := filepath.Join(t.TempDir(), "w.mp4")
	always := newRemote(size, -1)
	short := func(ctx context.Context, off int64) ([]byte, error) {
		b, err := always.fetch(ctx, off)
		return b[:len(b)/2], err
	}
	if _, err := downloadFile(context.Background(), final2, size, "k", 1, short, func(int64) {}); err == nil {
		t.Fatal("persistently short blocks must fail")
	}
}

func TestDownloadFinishesFromCompleteState(t *testing.T) {
	final := filepath.Join(t.TempDir(), "v.mp4")
	size := int64(partSize + 5)
	r := newRemote(size, -1)
	// simulate a crash after all blocks were written but before the rename
	if err := os.WriteFile(final+partExt, r.data, 0o644); err != nil {
		t.Fatal(err)
	}
	st := &partState{Format: stateFileFormat, Size: size, Key: "k", Done: []byte{0b11}}
	if err := saveState(final, st); err != nil {
		t.Fatal(err)
	}
	resumed, err := downloadFile(context.Background(), final, size, "k", 4, r.fetch, func(int64) {})
	if err != nil || !resumed || r.n != 0 {
		t.Fatalf("err=%v resumed=%v fetches=%d", err, resumed, r.n)
	}
	if got, _ := os.ReadFile(final); !bytes.Equal(got, r.data) {
		t.Fatal("content mismatch")
	}
}

func TestDownloadEmptyFile(t *testing.T) {
	final := filepath.Join(t.TempDir(), "empty.bin")
	if _, err := downloadFile(context.Background(), final, 0, "k", 4, newRemote(0, -1).fetch, func(int64) {}); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(final); err != nil || st.Size() != 0 {
		t.Fatalf("empty file: %v %v", st, err)
	}
}
