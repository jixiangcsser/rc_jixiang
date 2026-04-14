package server

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"rc_jixiang/internal/model"
	"rc_jixiang/internal/store"
	"time"
)

type Handler struct {
	store  *store.Store
	logger *slog.Logger
}

func New(s *store.Store, logger *slog.Logger) http.Handler {
	h := &Handler{store: s, logger: logger}
	mux := http.NewServeMux()
	// Go 1.22 起 ServeMux 支持方法前缀和路径参数（{id}），无需引入第三方路由库
	mux.HandleFunc("POST /notifications", h.createNotification)
	mux.HandleFunc("GET /notifications/{id}", h.getNotification)
	mux.HandleFunc("GET /notifications", h.listNotifications)
	mux.HandleFunc("GET /dead-letters", h.listDeadLetters)
	mux.HandleFunc("GET /health", h.health)
	return mux
}

type createRequest struct {
	VendorID    string            `json:"vendor_id"`
	URL         string            `json:"url"`
	Method      string            `json:"method"`
	Headers     map[string]string `json:"headers"`
	Body        string            `json:"body"`
	MaxAttempts int               `json:"max_attempts"`
}

// createNotification 接收通知任务，立即持久化后返回 202 Accepted。
//
// 关键设计：写库成功即返回，投递完全异步。
// 调用方不需要、也不应该等待外部 API 的响应结果。
func (h *Handler) createNotification(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.VendorID == "" || req.URL == "" {
		writeError(w, http.StatusBadRequest, "vendor_id and url are required")
		return
	}

	// 填充默认值，允许调用方省略非必填字段
	if req.Method == "" {
		req.Method = "POST"
	}
	if req.MaxAttempts <= 0 {
		req.MaxAttempts = 5
	}
	if req.Headers == nil {
		req.Headers = map[string]string{}
	}

	id, err := newUUID()
	if err != nil {
		h.logger.Error("create notification uuid", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	now := time.Now().UTC()
	n := &model.Notification{
		ID:          id,
		VendorID:    req.VendorID,
		URL:         req.URL,
		Method:      req.Method,
		Headers:     req.Headers,
		Body:        req.Body,
		Status:      model.StatusPending,
		Attempts:    0,
		MaxAttempts: req.MaxAttempts,
		// NextRetryAt 设为当前时间，使任务在下次 Dispatcher 轮询时立即被认领
		NextRetryAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	if err := h.store.Insert(r.Context(), n); err != nil {
		h.logger.Error("create notification insert", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]string{
		"id":      id,
		"status":  string(model.StatusPending),
		"message": "notification accepted",
	})
}

func (h *Handler) getNotification(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing id")
		return
	}

	n, err := h.store.GetByID(r.Context(), id)
	if err != nil {
		h.logger.Error("get notification", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if n == nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}

	writeJSON(w, http.StatusOK, n)
}

// listNotifications 支持按 status 过滤，默认返回 pending 状态的任务。
// 常用场景：运维排查时快速查看积压任务（status=pending）或失败任务（status=failed）。
func (h *Handler) listNotifications(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	if status == "" {
		status = "pending"
	}

	notifications, err := h.store.ListByStatus(r.Context(), status)
	if err != nil {
		h.logger.Error("list notifications", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	// 返回空数组而非 null，避免调用方需要处理 null 的情况
	if notifications == nil {
		notifications = []*model.Notification{}
	}

	writeJSON(w, http.StatusOK, notifications)
}

func (h *Handler) listDeadLetters(w http.ResponseWriter, r *http.Request) {
	dls, err := h.store.ListDeadLetters(r.Context())
	if err != nil {
		h.logger.Error("list dead letters", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if dls == nil {
		dls = []*model.DeadLetter{}
	}

	writeJSON(w, http.StatusOK, dls)
}

func (h *Handler) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// newUUID 生成 RFC 4122 v4 UUID（随机）
func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("newUUID: %w", err)
	}
	// version = 4
	b[6] = (b[6] & 0x0f) | 0x40
	// variant = RFC 4122
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
