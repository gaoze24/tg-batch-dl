// Package web serves the local UI and its JSON API.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"strconv"
	"time"

	"go.uber.org/zap"

	"github.com/gaoze24/tg-batch-dl/internal/config"
	"github.com/gaoze24/tg-batch-dl/internal/jobs"
	"github.com/gaoze24/tg-batch-dl/internal/sysutil"
	"github.com/gaoze24/tg-batch-dl/internal/tgc"
)

//go:embed static
var staticFS embed.FS

const mediaPageSize = 60

type Server struct {
	Version string
	Port    int
	TG      *tgc.Client
	Jobs    *jobs.Manager
	Config  *config.Store
	Quit    func()
	Log     *zap.Logger
}

func (s *Server) Handler() http.Handler {
	static, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err)
	}
	mux := http.NewServeMux()
	mux.Handle("GET /", http.FileServerFS(static))

	mux.HandleFunc("GET /api/state", s.state)
	mux.HandleFunc("POST /api/login/qr", s.loginQR)
	mux.HandleFunc("GET /api/login/qr.png", s.loginQRImage)
	mux.HandleFunc("POST /api/login/phone", s.loginPhone)
	mux.HandleFunc("POST /api/login/code", s.loginCode)
	mux.HandleFunc("POST /api/login/password", s.loginPassword)
	mux.HandleFunc("POST /api/login/cancel", s.loginCancel)
	mux.HandleFunc("POST /api/logout", s.logout)

	mux.HandleFunc("GET /api/chats", s.chats)
	mux.HandleFunc("GET /api/chats/{ref}/media", s.media)
	mux.HandleFunc("GET /api/thumb/{ref}/{id}", s.thumb)

	mux.HandleFunc("GET /api/jobs", s.listJobs)
	mux.HandleFunc("POST /api/jobs", s.createJobs)
	mux.HandleFunc("POST /api/jobs/clear", s.clearJobs)
	mux.HandleFunc("POST /api/jobs/{id}/cancel", s.cancelJob)
	mux.HandleFunc("POST /api/jobs/{id}/retry", s.retryJob)
	mux.HandleFunc("POST /api/jobs/{id}/restart", s.restartJob)
	mux.HandleFunc("POST /api/jobs/{id}/open", s.openJob)

	mux.HandleFunc("GET /api/settings", s.getSettings)
	mux.HandleFunc("POST /api/settings", s.putSettings)
	mux.HandleFunc("POST /api/open-dir", s.openDir)
	mux.HandleFunc("POST /api/quit", s.quit)
	return guard(s.Port, mux)
}

// ---- helpers ----

func readJSON(w http.ResponseWriter, r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return errors.New("请求格式不对")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func ok(w http.ResponseWriter) { writeJSON(w, http.StatusOK, map[string]bool{"ok": true}) }

func statusFor(err error) int {
	switch {
	case errors.Is(err, tgc.ErrNotReady):
		return http.StatusServiceUnavailable
	case errors.Is(err, tgc.ErrUnknownChat), errors.Is(err, tgc.ErrNoLogin):
		return http.StatusNotFound
	}
	return http.StatusBadRequest
}

func apiCtx(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), d)
}

// ---- state & login ----

func (s *Server) state(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"version":      s.Version,
		"telegram":     s.TG.Status(),
		"download_dir": s.Config.Get().DownloadDir,
	})
}

func (s *Server) loginQR(w http.ResponseWriter, _ *http.Request) {
	if err := s.TG.StartQR(); err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	ok(w)
}

func (s *Server) loginQRImage(w http.ResponseWriter, _ *http.Request) {
	png, err := s.TG.QRImage()
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(png)
}

func (s *Server) loginPhone(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Phone string `json:"phone"`
	}
	if err := readJSON(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.TG.StartPhone(body.Phone); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ok(w)
}

func (s *Server) loginCode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Code string `json:"code"`
	}
	if err := readJSON(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.TG.SubmitCode(body.Code); err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	ok(w)
}

func (s *Server) loginPassword(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password string `json:"password"`
	}
	if err := readJSON(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.TG.SubmitPassword(body.Password); err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	ok(w)
}

func (s *Server) loginCancel(w http.ResponseWriter, _ *http.Request) {
	s.TG.CancelLogin()
	ok(w)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := apiCtx(r, 20*time.Second)
	defer cancel()
	if err := s.TG.Logout(ctx); err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	ok(w)
}

// ---- browsing ----

func (s *Server) chats(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := apiCtx(r, 2*time.Minute)
	defer cancel()
	list, err := s.TG.Chats(ctx, r.URL.Query().Get("refresh") == "1")
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"chats": list})
}

func (s *Server) media(w http.ResponseWriter, r *http.Request) {
	ref := r.PathValue("ref")
	filter := r.URL.Query().Get("filter")
	if !tgc.ValidRef(ref) || !tgc.ValidFilter(filter) {
		writeErr(w, http.StatusBadRequest, errors.New("参数不对"))
		return
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	ctx, cancel := apiCtx(r, time.Minute)
	defer cancel()
	page, err := s.TG.Media(ctx, ref, filter, max(0, offset), mediaPageSize)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	if chat, err := s.TG.Peer(ctx, ref); err == nil {
		jobs.MarkDownloaded(s.Config.Get().DownloadDir, chat, page.Items)
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) thumb(w http.ResponseWriter, r *http.Request) {
	ref := r.PathValue("ref")
	id, err := strconv.Atoi(r.PathValue("id"))
	if !tgc.ValidRef(ref) || err != nil || id <= 0 {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := apiCtx(r, 30*time.Second)
	defer cancel()
	data, err := s.TG.Thumb(ctx, ref, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "private, max-age=86400")
	_, _ = w.Write(data)
}

// ---- jobs ----

func (s *Server) listJobs(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"jobs": s.Jobs.Views()})
}

type jobRequest struct {
	Ref    string   `json:"ref"`
	IDs    []int    `json:"ids"`
	All    bool     `json:"all"`
	Filter string   `json:"filter"`
	Links  []string `json:"links"`
}

func (s *Server) createJobs(w http.ResponseWriter, r *http.Request) {
	var req jobRequest
	if err := readJSON(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := apiCtx(r, 2*time.Minute)
	defer cancel()

	specs, err := s.specsFor(ctx, req)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	var created []jobs.View
	for _, spec := range specs {
		v, err := s.Jobs.Submit(ctx, spec)
		if err != nil {
			writeErr(w, statusFor(err), err)
			return
		}
		created = append(created, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": created})
}

// specsFor turns a request into one spec per chat (pasted links may span several chats).
func (s *Server) specsFor(ctx context.Context, req jobRequest) ([]jobs.Spec, error) {
	if len(req.Links) == 0 {
		if !tgc.ValidRef(req.Ref) {
			return nil, tgc.ErrUnknownChat
		}
		if len(req.IDs) > 100_000 {
			return nil, errors.New("一次最多选 100000 条")
		}
		return []jobs.Spec{{Ref: req.Ref, IDs: req.IDs, All: req.All, Filter: req.Filter}}, nil
	}
	if len(req.Links) > 1000 {
		return nil, errors.New("一次最多粘贴 1000 条链接")
	}
	byRef := map[string]*jobs.Spec{}
	var order []string
	for _, raw := range req.Links {
		link, err := tgc.ParseLink(raw)
		if err != nil {
			return nil, err
		}
		chat, err := s.TG.ResolveLink(ctx, link)
		if err != nil {
			return nil, err
		}
		sp, ok := byRef[chat.Ref]
		if !ok {
			sp = &jobs.Spec{Ref: chat.Ref}
			byRef[chat.Ref] = sp
			order = append(order, chat.Ref)
		}
		sp.IDs = append(sp.IDs, link.MsgID)
	}
	out := make([]jobs.Spec, 0, len(order))
	for _, ref := range order {
		out = append(out, *byRef[ref])
	}
	return out, nil
}

func (s *Server) clearJobs(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]int{"removed": s.Jobs.ClearFinished()})
}

func (s *Server) cancelJob(w http.ResponseWriter, r *http.Request) {
	v, found := s.Jobs.Cancel(r.PathValue("id"))
	if !found {
		writeErr(w, http.StatusNotFound, errors.New("任务不存在"))
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) retryJob(w http.ResponseWriter, r *http.Request) {
	s.resubmit(w, r, s.Jobs.RetrySpec)
}

func (s *Server) restartJob(w http.ResponseWriter, r *http.Request) {
	s.resubmit(w, r, s.Jobs.RestartSpec)
}

func (s *Server) resubmit(w http.ResponseWriter, r *http.Request, specOf func(string) (jobs.Spec, error)) {
	spec, err := specOf(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := apiCtx(r, time.Minute)
	defer cancel()
	v, err := s.Jobs.Submit(ctx, spec)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) openJob(w http.ResponseWriter, r *http.Request) {
	dest, found := s.Jobs.Dest(r.PathValue("id"))
	if !found {
		writeErr(w, http.StatusNotFound, errors.New("任务不存在"))
		return
	}
	if err := sysutil.OpenDir(dest); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ok(w)
}

// ---- settings & app ----

func (s *Server) getSettings(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.Config.Get())
}

func (s *Server) putSettings(w http.ResponseWriter, r *http.Request) {
	var next config.Settings
	if err := readJSON(w, r, &next); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	saved, err := s.Config.Update(next)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, saved)
}

func (s *Server) openDir(w http.ResponseWriter, _ *http.Request) {
	if err := sysutil.OpenDir(s.Config.Get().DownloadDir); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ok(w)
}

func (s *Server) quit(w http.ResponseWriter, _ *http.Request) {
	ok(w)
	if s.Quit != nil {
		go func() {
			time.Sleep(300 * time.Millisecond) // let the response reach the page first
			s.Quit()
		}()
	}
}
