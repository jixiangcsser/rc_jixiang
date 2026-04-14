package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"rc_jixiang/internal/model"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS notifications (
    id TEXT PRIMARY KEY,
    vendor_id TEXT NOT NULL,
    url TEXT NOT NULL,
    method TEXT NOT NULL DEFAULT 'POST',
    headers TEXT NOT NULL DEFAULT '{}',
    body TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'pending',
    attempts INTEGER NOT NULL DEFAULT 0,
    max_attempts INTEGER NOT NULL DEFAULT 5,
    next_retry_at DATETIME NOT NULL,
    created_at DATETIME NOT NULL,
    updated_at DATETIME NOT NULL,
    last_error TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS dead_letters (
    id TEXT PRIMARY KEY,
    notification_id TEXT NOT NULL,
    vendor_id TEXT NOT NULL,
    url TEXT NOT NULL,
    last_error TEXT NOT NULL,
    attempts INTEGER NOT NULL,
    created_at DATETIME NOT NULL
);

-- 复合索引：Dispatcher 轮询时按 (status, next_retry_at) 过滤并排序，此索引使查询走 index scan
CREATE INDEX IF NOT EXISTS idx_status_retry ON notifications(status, next_retry_at);
`

func New(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("store.New open: %w", err)
	}

	// SQLite 默认只允许一个写连接。设为 1 避免 "database is locked" 错误。
	// 若未来迁移至 PostgreSQL，可移除此限制。
	db.SetMaxOpenConns(1)

	// WAL 模式允许读写并发：读不阻塞写，写不阻塞读。
	// 相比默认的 DELETE journal，WAL 在高频写入下性能更好，且进程崩溃后可安全恢复。
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("store.New WAL: %w", err)
	}

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("store.New schema: %w", err)
	}

	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

// RecoverProcessing 在服务启动时调用，将所有卡在 processing 状态的任务重置为 pending。
//
// 场景：进程在投递成功后、写入 done 之前崩溃，任务会停留在 processing。
// 重置后任务会被重新投递，这是 at-least-once 语义的核心保障，代价是可能出现重复投递。
func (s *Store) RecoverProcessing(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE notifications SET status='pending' WHERE status='processing'`)
	if err != nil {
		return fmt.Errorf("store.RecoverProcessing: %w", err)
	}
	return nil
}

func (s *Store) Insert(ctx context.Context, n *model.Notification) error {
	headers, err := json.Marshal(n.Headers)
	if err != nil {
		return fmt.Errorf("store.Insert marshal headers: %w", err)
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO notifications (id, vendor_id, url, method, headers, body, status, attempts, max_attempts, next_retry_at, created_at, updated_at, last_error)
         VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		n.ID, n.VendorID, n.URL, n.Method, string(headers), n.Body,
		string(n.Status), n.Attempts, n.MaxAttempts,
		n.NextRetryAt.Format(time.RFC3339Nano),
		n.CreatedAt.Format(time.RFC3339Nano),
		n.UpdatedAt.Format(time.RFC3339Nano),
		n.LastError,
	)
	if err != nil {
		return fmt.Errorf("store.Insert: %w", err)
	}
	return nil
}

func (s *Store) GetByID(ctx context.Context, id string) (*model.Notification, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, vendor_id, url, method, headers, body, status, attempts, max_attempts, next_retry_at, created_at, updated_at, last_error
         FROM notifications WHERE id = ?`, id)
	n, err := scanNotification(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store.GetByID: %w", err)
	}
	return n, nil
}

func (s *Store) ListByStatus(ctx context.Context, status string) ([]*model.Notification, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, vendor_id, url, method, headers, body, status, attempts, max_attempts, next_retry_at, created_at, updated_at, last_error
         FROM notifications WHERE status = ? ORDER BY created_at DESC LIMIT 100`, status)
	if err != nil {
		return nil, fmt.Errorf("store.ListByStatus: %w", err)
	}
	defer rows.Close()
	return scanNotifications(rows)
}

// ClaimPending 以事务原子性地将 pending 任务标记为 processing，并返回这批任务。
//
// 原子性设计：
//   - 先在事务内 SELECT 出待处理任务，再逐条 UPDATE 为 processing，最后 Commit。
//   - 若 Commit 失败（如进程崩溃），任务保持 pending，下次轮询时会被重新认领。
//   - 只取 next_retry_at <= now 的任务，确保退避时间窗口内的任务不会被提前重试。
//
// limit 参数控制单次认领数量，与 workerCount 对齐，避免任务数远超 Worker 导致队列积压。
func (s *Store) ClaimPending(ctx context.Context, limit int) ([]*model.Notification, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store.ClaimPending begin: %w", err)
	}
	defer tx.Rollback()

	now := time.Now().Format(time.RFC3339Nano)
	rows, err := tx.QueryContext(ctx,
		`SELECT id, vendor_id, url, method, headers, body, status, attempts, max_attempts, next_retry_at, created_at, updated_at, last_error
         FROM notifications
         WHERE status = 'pending' AND next_retry_at <= ?
         ORDER BY next_retry_at ASC
         LIMIT ?`, now, limit)
	if err != nil {
		return nil, fmt.Errorf("store.ClaimPending select: %w", err)
	}

	var ids []string
	var notifications []*model.Notification
	for rows.Next() {
		n, err := scanNotification(rows)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("store.ClaimPending scan: %w", err)
		}
		ids = append(ids, n.ID)
		notifications = append(notifications, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.ClaimPending rows: %w", err)
	}

	updatedAt := time.Now().Format(time.RFC3339Nano)
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx,
			`UPDATE notifications SET status='processing', updated_at=? WHERE id=?`,
			updatedAt, id); err != nil {
			return nil, fmt.Errorf("store.ClaimPending update: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store.ClaimPending commit: %w", err)
	}

	// Commit 成功后，更新内存中对象的状态，与数据库保持一致
	for _, n := range notifications {
		n.Status = model.StatusProcessing
	}
	return notifications, nil
}

func (s *Store) MarkDone(ctx context.Context, id string) error {
	now := time.Now().Format(time.RFC3339Nano)
	_, err := s.db.ExecContext(ctx,
		`UPDATE notifications SET status='done', updated_at=? WHERE id=?`, now, id)
	if err != nil {
		return fmt.Errorf("store.MarkDone: %w", err)
	}
	return nil
}

// MarkFailed 处理投递失败的两种情况：
//   - moveToDeadLetter=false：重试次数未耗尽，重置为 pending 并更新 next_retry_at，等待退避后重试
//   - moveToDeadLetter=true：重试次数耗尽，将任务标记为 failed 并写入 dead_letters 表，不再自动重试
func (s *Store) MarkFailed(ctx context.Context, id, lastError string, nextRetry time.Time, moveToDeadLetter bool) error {
	if moveToDeadLetter {
		return s.markFailedDeadLetter(ctx, id, lastError)
	}
	return s.markFailedRetry(ctx, id, lastError, nextRetry)
}

// markFailedDeadLetter 在事务内同时完成两件事：
// 1. 向 dead_letters 表插入一条记录（供运维排查）
// 2. 将 notifications 表中的任务标记为 failed（停止调度）
// 使用事务确保两者要么都成功，要么都失败，不会出现只有一半数据落库的状态。
func (s *Store) markFailedDeadLetter(ctx context.Context, id, lastError string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store.MarkFailed begin: %w", err)
	}
	defer tx.Rollback()

	row := tx.QueryRowContext(ctx,
		`SELECT id, vendor_id, url, attempts FROM notifications WHERE id=?`, id)
	var nid, vendorID, url string
	var attempts int
	if err := row.Scan(&nid, &vendorID, &url, &attempts); err != nil {
		return fmt.Errorf("store.MarkFailed scan: %w", err)
	}

	dlID, err := newUUID()
	if err != nil {
		return fmt.Errorf("store.MarkFailed uuid: %w", err)
	}
	now := time.Now().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO dead_letters (id, notification_id, vendor_id, url, last_error, attempts, created_at)
         VALUES (?, ?, ?, ?, ?, ?, ?)`,
		dlID, nid, vendorID, url, lastError, attempts+1, now); err != nil {
		return fmt.Errorf("store.MarkFailed insert dead_letter: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE notifications SET status='failed', attempts=attempts+1, last_error=?, updated_at=? WHERE id=?`,
		lastError, now, id); err != nil {
		return fmt.Errorf("store.MarkFailed update: %w", err)
	}

	return tx.Commit()
}

// markFailedRetry 将任务重置为 pending，更新 next_retry_at 为退避后的时间点。
// Dispatcher 下次轮询时，只有 next_retry_at <= now 的任务才会被认领，从而实现退避等待。
func (s *Store) markFailedRetry(ctx context.Context, id, lastError string, nextRetry time.Time) error {
	now := time.Now().Format(time.RFC3339Nano)
	_, err := s.db.ExecContext(ctx,
		`UPDATE notifications SET status='pending', attempts=attempts+1, last_error=?, next_retry_at=?, updated_at=? WHERE id=?`,
		lastError, nextRetry.Format(time.RFC3339Nano), now, id)
	if err != nil {
		return fmt.Errorf("store.markFailedRetry: %w", err)
	}
	return nil
}

func (s *Store) ListDeadLetters(ctx context.Context) ([]*model.DeadLetter, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, notification_id, vendor_id, url, last_error, attempts, created_at FROM dead_letters ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("store.ListDeadLetters: %w", err)
	}
	defer rows.Close()

	var result []*model.DeadLetter
	for rows.Next() {
		dl := &model.DeadLetter{}
		var createdAt string
		if err := rows.Scan(&dl.ID, &dl.NotificationID, &dl.VendorID, &dl.URL, &dl.LastError, &dl.Attempts, &createdAt); err != nil {
			return nil, fmt.Errorf("store.ListDeadLetters scan: %w", err)
		}
		dl.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
		result = append(result, dl)
	}
	return result, rows.Err()
}

// scanner 是 *sql.Row 和 *sql.Rows 的公共接口，允许 scanNotification 同时处理单行和多行查询
type scanner interface {
	Scan(dest ...any) error
}

func scanNotification(row scanner) (*model.Notification, error) {
	n := &model.Notification{}
	var headers, nextRetryAt, createdAt, updatedAt, status string
	err := row.Scan(
		&n.ID, &n.VendorID, &n.URL, &n.Method, &headers, &n.Body,
		&status, &n.Attempts, &n.MaxAttempts,
		&nextRetryAt, &createdAt, &updatedAt, &n.LastError,
	)
	if err != nil {
		return nil, err
	}
	n.Status = model.Status(status)
	// headers 以 JSON 字符串存储在 SQLite，反序列化失败时降级为空 map，不中断流程
	if err := json.Unmarshal([]byte(headers), &n.Headers); err != nil {
		n.Headers = map[string]string{}
	}
	n.NextRetryAt, _ = time.Parse(time.RFC3339Nano, nextRetryAt)
	n.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
	n.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedAt)
	return n, nil
}

func scanNotifications(rows *sql.Rows) ([]*model.Notification, error) {
	var result []*model.Notification
	for rows.Next() {
		n, err := scanNotification(rows)
		if err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		result = append(result, n)
	}
	return result, rows.Err()
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
