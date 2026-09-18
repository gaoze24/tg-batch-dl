package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

// Resumable file download. The file is fetched in 1 MB blocks (the largest upload.getFile allows) into
// "<final>.part"; "<final>.part.state" records which blocks are safely on disk, so an interrupted download continues
// where it stopped instead of starting over. tdl's own downloader can't do this: it always starts at offset 0.
const (
	partSize        = 1 << 20
	partExt         = ".part"
	stateExt        = ".part.state"
	stateSaveEvery  = 2 * time.Second
	shortReadTries  = 3
	stateFileFormat = 1
)

// fetchFunc returns the block at offset (a multiple of partSize): partSize bytes, or fewer for the last block.
type fetchFunc func(ctx context.Context, offset int64) ([]byte, error)

type partState struct {
	Format int    `json:"format"`
	Size   int64  `json:"size"`
	Key    string `json:"key"`  // identifies the remote file, so a different file never resumes this one
	Done   []byte `json:"done"` // bitmap of finished blocks
}

func blocks(size int64) int { return int((size + partSize - 1) / partSize) }

func (s *partState) has(i int) bool { return s.Done[i/8]&(1<<(i%8)) != 0 }
func (s *partState) set(i int)      { s.Done[i/8] |= 1 << (i % 8) }

func blockLen(size int64, i int) int64 { return min(partSize, size-int64(i)*partSize) }

// loadState returns the saved progress for final if it matches size and key and the .part file is still there.
func loadState(final string, size int64, key string) (*partState, bool) {
	data, err := os.ReadFile(final + stateExt)
	if err != nil {
		return nil, false
	}
	var s partState
	if json.Unmarshal(data, &s) != nil || s.Format != stateFileFormat || s.Size != size || s.Key != key || len(s.Done) != (blocks(size)+7)/8 {
		return nil, false
	}
	if st, err := os.Stat(final + partExt); err != nil || st.Size() > size {
		return nil, false
	}
	return &s, true
}

func saveState(final string, s *partState) error {
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	tmp := final + stateExt + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, final+stateExt)
}

// downloadFile fetches size bytes into final, resuming a previous attempt when possible. progress receives the number
// of bytes on disk (including resumed ones). On error the partial file and its state are kept for the next attempt.
// It reports resumed=true when earlier progress was reused.
func downloadFile(ctx context.Context, final string, size int64, key string, threads int, fetch fetchFunc, progress func(done int64)) (resumed bool, err error) {
	if size == 0 {
		return false, os.WriteFile(final, nil, 0o644)
	}
	state, ok := loadState(final, size, key)
	flags := os.O_RDWR | os.O_CREATE
	if ok {
		resumed = true
	} else {
		state = &partState{Format: stateFileFormat, Size: size, Key: key, Done: make([]byte, (blocks(size)+7)/8)}
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(final+partExt, flags, 0o644)
	if err != nil {
		return false, err
	}

	var (
		mu    sync.Mutex
		done  int64
		todo  []int
		dirty bool
	)
	for i := range blocks(size) {
		if state.has(i) {
			done += blockLen(size, i)
		} else {
			todo = append(todo, i)
		}
	}
	progress(done)

	// persist progress: sync the data first, so the state never claims blocks that aren't on disk
	checkpoint := func() error {
		mu.Lock()
		if !dirty {
			mu.Unlock()
			return nil
		}
		snap := &partState{Format: state.Format, Size: state.Size, Key: state.Key, Done: append([]byte(nil), state.Done...)}
		dirty = false
		mu.Unlock()
		if err := f.Sync(); err != nil {
			return err
		}
		return saveState(final, snap)
	}

	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	queue := make(chan int, len(todo))
	for _, i := range todo {
		queue <- i
	}
	close(queue)

	var (
		wg       sync.WaitGroup
		errOnce  sync.Once
		firstErr error
	)
	fail := func(e error) {
		errOnce.Do(func() { firstErr = e })
		cancel()
	}
	for range max(1, min(threads, len(todo))) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range queue {
				if wctx.Err() != nil {
					return
				}
				off := int64(i) * partSize
				want := blockLen(size, i)
				var b []byte
				for try := 1; ; try++ {
					var err error
					if b, err = fetch(wctx, off); err != nil {
						fail(err)
						return
					}
					if int64(len(b)) == want {
						break
					}
					if try >= shortReadTries {
						fail(fmt.Errorf("第 %d 块数据长度不对（%d/%d 字节）", i, len(b), want))
						return
					}
				}
				if _, err := f.WriteAt(b, off); err != nil {
					fail(err)
					return
				}
				mu.Lock()
				state.set(i)
				dirty = true
				done += want
				d := done
				mu.Unlock()
				progress(d)
			}
		}()
	}

	stopSaver, saverExited := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(saverExited)
		t := time.NewTicker(stateSaveEvery)
		defer t.Stop()
		for {
			select {
			case <-stopSaver:
				return
			case <-t.C:
				_ = checkpoint()
			}
		}
	}()
	wg.Wait()
	close(stopSaver)
	<-saverExited // no checkpoint may run concurrently with the final one below

	complete := done == size
	if !complete {
		cpErr := checkpoint()
		closeErr := f.Close()
		err := firstErr
		if err == nil {
			err = ctx.Err()
		}
		if err == nil {
			err = errors.Join(errors.New("下载没有完成"), cpErr, closeErr)
		}
		return resumed, err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return resumed, err
	}
	if err := f.Close(); err != nil {
		return resumed, err
	}
	if err := os.Rename(final+partExt, final); err != nil {
		return resumed, err
	}
	_ = os.Remove(final + stateExt)
	return resumed, nil
}
