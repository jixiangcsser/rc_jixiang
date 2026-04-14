package server

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"rc_jixiang/internal/store"
)

func newTestHandler(t *testing.T) http.Handler {
	t.Helper()
	s, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return New(s, logger)
}

func TestHealth(t *testing.T) {
	h := newTestHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("want 200, got %d", rec.Code)
	}
}

func TestCreateNotification_Success(t *testing.T) {
	h := newTestHandler(t)
	body := `{"vendor_id":"crm","url":"https://example.com/hook","body":"{}"}`
	req := httptest.NewRequest(http.MethodPost, "/notifications", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Errorf("want 202, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["id"] == "" {
		t.Error("expected non-empty id")
	}
	if resp["status"] != "pending" {
		t.Errorf("want status=pending, got %s", resp["status"])
	}
}

func TestCreateNotification_MissingFields(t *testing.T) {
	h := newTestHandler(t)

	cases := []struct {
		name string
		body string
	}{
		{"missing vendor_id", `{"url":"https://example.com"}`},
		{"missing url", `{"vendor_id":"crm"}`},
		{"empty body", `{}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/notifications", bytes.NewBufferString(tc.body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("want 400, got %d", rec.Code)
			}
		})
	}
}

func TestCreateNotification_InvalidJSON(t *testing.T) {
	h := newTestHandler(t)
	req := httptest.NewRequest(http.MethodPost, "/notifications", bytes.NewBufferString("not-json"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", rec.Code)
	}
}

func TestGetNotification_NotFound(t *testing.T) {
	h := newTestHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/notifications/nonexistent", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", rec.Code)
	}
}

func TestGetNotification_Found(t *testing.T) {
	h := newTestHandler(t)

	// create a notification
	body := `{"vendor_id":"v1","url":"https://example.com"}`
	req := httptest.NewRequest(http.MethodPost, "/notifications", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("create failed: %d %s", rec.Code, rec.Body.String())
	}

	var createResp map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&createResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	id := createResp["id"]

	// fetch it
	req2 := httptest.NewRequest(http.MethodGet, "/notifications/"+id, nil)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Errorf("want 200, got %d: %s", rec2.Code, rec2.Body.String())
	}

	var n map[string]any
	if err := json.NewDecoder(rec2.Body).Decode(&n); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if n["ID"] != id {
		t.Errorf("want ID=%s, got %v", id, n["ID"])
	}
}

func TestListNotifications(t *testing.T) {
	h := newTestHandler(t)

	for range 3 {
		body := `{"vendor_id":"v","url":"https://example.com"}`
		req := httptest.NewRequest(http.MethodPost, "/notifications", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("create failed: %d", rec.Code)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/notifications?status=pending", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("want 200, got %d", rec.Code)
	}

	var list []any
	if err := json.NewDecoder(rec.Body).Decode(&list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list) != 3 {
		t.Errorf("want 3 items, got %d", len(list))
	}
}

// TC: 未传 method / max_attempts 时服务端应填充默认值
func TestCreateNotification_Defaults(t *testing.T) {
	h := newTestHandler(t)
	body := `{"vendor_id":"v","url":"https://example.com"}`
	req := httptest.NewRequest(http.MethodPost, "/notifications", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("create failed: %d", rec.Code)
	}

	var resp map[string]string
	json.NewDecoder(rec.Body).Decode(&resp)
	id := resp["id"]

	req2 := httptest.NewRequest(http.MethodGet, "/notifications/"+id, nil)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)

	var n map[string]any
	json.NewDecoder(rec2.Body).Decode(&n)
	if n["Method"] != "POST" {
		t.Errorf("want default method=POST, got %v", n["Method"])
	}
	if n["MaxAttempts"] != float64(5) {
		t.Errorf("want default max_attempts=5, got %v", n["MaxAttempts"])
	}
}

// TC: GET /notifications 不带 status 参数，默认返回 pending 列表
func TestListNotifications_DefaultStatusPending(t *testing.T) {
	h := newTestHandler(t)
	body := `{"vendor_id":"v","url":"https://example.com"}`
	req := httptest.NewRequest(http.MethodPost, "/notifications", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("create failed: %d", rec.Code)
	}

	// 不带 status 参数
	req2 := httptest.NewRequest(http.MethodGet, "/notifications", nil)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Errorf("want 200, got %d", rec2.Code)
	}

	var list []any
	json.NewDecoder(rec2.Body).Decode(&list)
	if len(list) != 1 {
		t.Errorf("want 1 pending item (default), got %d", len(list))
	}
}

func TestListDeadLetters_Empty(t *testing.T) {
	h := newTestHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/dead-letters", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("want 200, got %d", rec.Code)
	}

	var list []any
	if err := json.NewDecoder(rec.Body).Decode(&list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("want empty list, got %d items", len(list))
	}
}
