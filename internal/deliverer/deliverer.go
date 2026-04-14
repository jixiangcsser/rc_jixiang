package deliverer

import (
	"context"
	"fmt"
	"net/http"
	"rc_jixiang/internal/model"
	"strings"
	"time"
)

type Deliverer struct {
	client *http.Client
}

// New 创建 Deliverer，timeout 控制单次 HTTP 请求的最长等待时间。
// 超时后返回错误，Dispatcher 会按退避策略重试。
func New(timeout time.Duration) *Deliverer {
	return &Deliverer{
		client: &http.Client{Timeout: timeout},
	}
}

// Deliver 向外部供应商 API 发起一次 HTTP 请求。
//
// 成功条件：HTTP 响应状态码为 2xx。
// 失败处理：非 2xx 或网络错误均返回 error，由调用方决定是否重试。
// 设计原则：本方法只负责"发一次"，不感知重试逻辑，职责边界清晰。
func (d *Deliverer) Deliver(ctx context.Context, n *model.Notification) error {
	method := n.Method
	if method == "" {
		method = "POST"
	}

	req, err := http.NewRequestWithContext(ctx, method, n.URL, strings.NewReader(n.Body))
	if err != nil {
		return fmt.Errorf("deliverer.Deliver new request: %w", err)
	}

	// 将调用方指定的 headers 原样透传，本服务不添加也不修改任何 header，
	// 确保对不同供应商的认证方式（Bearer Token、API Key、签名等）完全透明
	for k, v := range n.Headers {
		req.Header.Set(k, v)
	}

	resp, err := d.client.Do(req)
	if err != nil {
		return fmt.Errorf("deliverer.Deliver do: %w", err)
	}
	defer resp.Body.Close()

	// 只关心状态码，不解析响应 body——业务系统不需要供应商的返回值，
	// 解析反而会引入对供应商响应格式的耦合
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("deliverer.Deliver: non-2xx status %d", resp.StatusCode)
	}

	return nil
}
