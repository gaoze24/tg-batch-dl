package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/gaoze24/tg-batch-dl/internal/config"
	"github.com/gaoze24/tg-batch-dl/internal/jobs"
	"github.com/gaoze24/tg-batch-dl/internal/tgc"
)

const testPort = 17800

func newHandler(t *testing.T) http.Handler {
	t.Helper()
	dir := t.TempDir()
	store, err := config.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	client := tgc.New(filepath.Join(dir, "session.json"), "", 0, "", zap.NewNop()) // never Run: stays "connecting"
	s := &Server{
		Version: "test",
		Port:    testPort,
		TG:      client,
		Jobs:    jobs.NewManager(client, store.Get, zap.NewNop()),
		Config:  store,
		Log:     zap.NewNop(),
	}
	return s.Handler()
}

func do(h http.Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://127.0.0.1:17800"+path, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

var api = map[string]string{"X-TGDL": "1", "Sec-Fetch-Site": "same-origin"}

func TestGuard(t *testing.T) {
	h := newHandler(t)

	req := httptest.NewRequest(http.MethodGet, "http://evil.example/api/state", nil)
	req.Header.Set("X-TGDL", "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("foreign host: %d", rec.Code)
	}

	if rec := do(h, "POST", "/api/quit", "", map[string]string{"X-TGDL": "1", "Sec-Fetch-Site": "cross-site"}); rec.Code != http.StatusForbidden {
		t.Errorf("cross-site: %d", rec.Code)
	}
	if rec := do(h, "GET", "/api/state", "", nil); rec.Code != http.StatusForbidden {
		t.Errorf("missing header: %d", rec.Code)
	}
	rec = do(h, "GET", "/api/state", "", api)
	if rec.Code != http.StatusOK {
		t.Fatalf("state: %d %s", rec.Code, rec.Body)
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("csp = %q", csp)
	}
	var st struct {
		Telegram tgc.Status `json:"telegram"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil || st.Telegram.State != tgc.StateConnecting {
		t.Errorf("state body %s (%v)", rec.Body, err)
	}

	// images can't send custom headers: allowed through the guard, then 404 because we're not logged in
	if rec := do(h, "GET", "/api/thumb/c1/5", "", map[string]string{"Sec-Fetch-Site": "same-origin"}); rec.Code != http.StatusNotFound {
		t.Errorf("thumb: %d", rec.Code)
	}
}

func TestIndexServed(t *testing.T) {
	rec := do(newHandler(t), "GET", "/", "", map[string]string{"Sec-Fetch-Site": "none"})
	if rec.Code != http.StatusOK || !strings.Contains(strings.ToLower(rec.Body.String()), "<!doctype html>") {
		t.Fatalf("index: %d", rec.Code)
	}
}

func TestSettingsValidation(t *testing.T) {
	h := newHandler(t)
	rec := do(h, "GET", "/api/settings", "", api)
	var s config.Settings
	if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil {
		t.Fatal(err)
	}
	s.Threads = 99
	body, _ := json.Marshal(s)
	if rec := do(h, "POST", "/api/settings", string(body), api); rec.Code != http.StatusBadRequest {
		t.Errorf("invalid settings accepted: %d", rec.Code)
	}
	s.Threads = 8
	body, _ = json.Marshal(s)
	if rec := do(h, "POST", "/api/settings", string(body), api); rec.Code != http.StatusOK {
		t.Errorf("valid settings rejected: %d %s", rec.Code, rec.Body)
	}
	if rec := do(h, "POST", "/api/settings", `{"unknown":1}`, api); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown fields accepted: %d", rec.Code)
	}
}

func TestJobRequestsNeedValidInput(t *testing.T) {
	h := newHandler(t)
	if rec := do(h, "POST", "/api/jobs", `{"ref":"x1","ids":[1]}`, api); rec.Code != http.StatusNotFound {
		t.Errorf("bad ref: %d", rec.Code)
	}
	if rec := do(h, "POST", "/api/jobs", `{"links":["https://evil.com/c/1/2"]}`, api); rec.Code != http.StatusBadRequest {
		t.Errorf("bad link: %d", rec.Code)
	}
	if rec := do(h, "GET", "/api/chats", "", api); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("chats before login: %d", rec.Code)
	}
	if rec := do(h, "POST", "/api/jobs/nope/cancel", "", api); rec.Code != http.StatusNotFound {
		t.Errorf("cancel unknown: %d", rec.Code)
	}
}
