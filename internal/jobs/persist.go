package jobs

import (
	"encoding/json"
	"errors"
	"os"
	"time"

	"go.uber.org/zap"
)

// saved is the on-disk form of a job (tgdl-data/jobs.json), enough to show it again and restart it.
type saved struct {
	ID        string   `json:"id"`
	Seq       int      `json:"seq"`
	Title     string   `json:"title"`
	Ref       string   `json:"ref"`
	IDs       []int    `json:"ids,omitempty"`
	All       bool     `json:"all,omitempty"`
	Filter    string   `json:"filter,omitempty"`
	Dest      string   `json:"dest"`
	Status    Status   `json:"status"`
	Message   string   `json:"message,omitempty"`
	Total     int      `json:"total"`
	Done      int      `json:"done"`
	Existing  int      `json:"existing"`
	Failed    int      `json:"failed"`
	FailedIDs []int    `json:"failed_ids,omitempty"`
	Errors    []string `json:"errors,omitempty"`
	Created   int64    `json:"created"`
	Ended     int64    `json:"ended,omitempty"`
}

// saveLocked writes all jobs; must be called with m.mu held. Failures are logged, never fatal.
func (m *Manager) saveLocked() {
	if m.path == "" {
		return
	}
	list := make([]saved, 0, len(m.jobs))
	for _, j := range m.jobs {
		s := saved{
			ID: j.id, Seq: j.seq, Title: j.title, Ref: j.spec.Ref, IDs: j.spec.IDs, All: j.spec.All, Filter: j.spec.Filter,
			Dest: j.dest, Status: j.status, Message: j.message,
			Total: j.total, Done: j.done, Existing: j.existing, Failed: j.failed, FailedIDs: j.failedID, Errors: j.errors,
			Created: j.created.Unix(),
		}
		if !j.ended.IsZero() {
			s.Ended = j.ended.Unix()
		}
		list = append(list, s)
	}
	data, err := json.Marshal(list)
	if err == nil {
		tmp := m.path + ".tmp"
		if err = os.WriteFile(tmp, data, 0o600); err == nil {
			err = os.Rename(tmp, m.path)
		}
	}
	if err != nil {
		m.log.Warn("save jobs", zap.Error(err))
	}
}

func (m *Manager) load() {
	if m.path == "" {
		return
	}
	data, err := os.ReadFile(m.path)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	var list []saved
	if err == nil {
		err = json.Unmarshal(data, &list)
	}
	if err != nil {
		m.log.Warn("load jobs", zap.Error(err))
		return
	}
	for _, s := range list {
		j := &job{
			id: s.ID, seq: s.Seq, title: s.Title, dest: s.Dest,
			spec:   Spec{Ref: s.Ref, IDs: s.IDs, All: s.All, Filter: s.Filter},
			status: s.Status, message: s.Message,
			total: s.Total, done: s.Done, existing: s.Existing, failed: s.Failed, failedID: s.FailedIDs, errors: s.Errors,
			active:  map[int64]*activeFile{},
			created: time.Unix(s.Created, 0),
		}
		if s.Ended != 0 {
			j.ended = time.Unix(s.Ended, 0)
		}
		if !j.status.finished() {
			j.status, j.message, j.ended = StatusInterrupted, "程序退出时中断了，可以点「重新开始」", time.Now()
		}
		m.jobs[j.id] = j
		m.seq = max(m.seq, j.seq)
	}
}
