package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"llmgateway/internal/apierr"
)

// Options 是构造适配器所需的连接参数。
type Options struct {
	BaseURL string        // 上游根地址，例如 http://127.0.0.1:9090
	APIKey  string        // 上游 API Key（从环境变量读取）
	Timeout time.Duration // 非流式调用的整体超时
}

// transport 封装适配器共用的 HTTP 细节（建连、超时、错误归一）。
// 三个适配器只在「请求体怎么拼、鉴权头怎么加、返回体怎么解」上有差异。
type transport struct {
	client  *http.Client
	baseURL string
	timeout time.Duration
}

func newTransport(opt Options) *transport {
	if opt.Timeout <= 0 {
		opt.Timeout = 60 * time.Second
	}
	return &transport{
		// 注意：client 本身不设 Timeout，否则会把流式响应从中间掐断；
		// 超时统一交给 per-call 的 context 控制。
		client: &http.Client{
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 20,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		baseURL: strings.TrimRight(opt.BaseURL, "/"),
		timeout: opt.Timeout,
	}
}

// post 发起一次 POST 请求。
// 流转顺序：① 序列化请求体 ② 装配鉴权/协议头 ③ 发送 ④ 非 2xx 归一成统一错误码
// 返回的 *http.Response 的 Body 由调用方负责关闭。
func (t *transport) post(ctx context.Context, path string, payload any, headers map[string]string) (*http.Response, *apierr.Error) {
	// ① 序列化
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, apierr.Wrap(err, apierr.CodeInternal, "序列化上游请求体失败")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.baseURL+path, bytes.NewReader(raw))
	if err != nil {
		return nil, apierr.Wrap(err, apierr.CodeInternal, "构造上游请求失败")
	}
	// ② 协议专属的头由各适配器传进来
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	// ③ 发送；网络层异常按超时/不可用分流
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, apierr.FromTransport(err)
	}
	// ④ 非 2xx：读出错误体后统一翻译，避免上游错误结构泄漏到上层
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, apierr.FromUpstreamStatus(resp.StatusCode, string(body))
	}
	return resp, nil
}

// callTimeout 为非流式调用附加超时 context。
func (t *transport) callTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, t.timeout)
}

// decodeJSON 读取并解析上游 JSON 响应，同时保留原始字节便于排查。
func decodeJSON(r io.Reader, out any) (json.RawMessage, *apierr.Error) {
	raw, err := io.ReadAll(io.LimitReader(r, 8<<20))
	if err != nil {
		return nil, apierr.FromTransport(err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return raw, apierr.Wrap(err, apierr.CodeUpstreamError, "解析上游响应失败: %s", err.Error())
	}
	return raw, nil
}

// firstNonEmpty 返回第一个非空字符串，用于字段回退。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
