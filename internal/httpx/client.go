// Package httpx 是招聘平台接入层的统一 HTTP 客户端。
//
// 它在标准库 net/http 之上叠加了一层「采集所需的公共行为」：
// 全局限速、robots.txt 网关、状态码与超时归一、指数退避重试、
// 浏览器默认头、cookie 日志脱敏。平台适配器只关心 URL 与响应结构。
package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"

	"jobworkspace/internal/errs"
	"jobworkspace/internal/ratelimit"
)

const (
	// defaultUA 是默认的桌面 Chrome User-Agent。
	defaultUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
	// defaultTimeout 是单次请求的默认超时。
	defaultTimeout = 15 * time.Second
	// defaultRobotsTTL 是 robots.txt 解析结果的默认缓存时长。
	defaultRobotsTTL = time.Hour
	// maxRedirects 是允许的最大重定向跳数。
	maxRedirects = 5
	// maxBodyBytes 是单次响应体的读取上限（10MB），防止异常响应打爆内存。
	maxBodyBytes = 10 << 20
	// snippetBytes 是错误消息里保留的响应体前缀长度。
	snippetBytes = 200
)

// errTooManyRedirects 是重定向超限的哨兵错误，用于与其它网络错误区分。
var errTooManyRedirects = errors.New("http: 重定向次数超过上限")

// Options 是构造 Client 的配置项。
type Options struct {
	// Cookie 是整条 Cookie 头的值；日志中只输出长度与首尾各 2 个字符。
	Cookie string
	// Headers 是额外请求头，最后写入，可覆盖所有默认头。
	Headers map[string]string
	// Referer 非空时写入 Referer 头。
	Referer string
	// UserAgent 覆盖默认的桌面 Chrome UA，同时用于匹配 robots.txt 的 User-agent 段。
	UserAgent string
	// Timeout 是单次请求超时，默认 15s。
	Timeout time.Duration
	// Retries 是失败后的最大重试次数（不含首次请求），0 表示不重试。
	Retries int
	// Limiter 为 nil 时不限速（不会 panic）。
	Limiter *ratelimit.Limiter
	// Log 为 nil 时丢弃全部日志。
	Log *slog.Logger
	// Platform 用于日志与错误信息标注来源平台。
	Platform string
	// RespectRobots 为 true 时执行 robots.txt 的 Disallow 规则。
	RespectRobots bool
	// AllowDisallowed 为 true 时即使命中 robots 规则也放行（仅告警）。
	AllowDisallowed bool
	// RobotsTTL 是 robots.txt 的缓存时长，默认 1h。
	RobotsTTL time.Duration
}

// Response 是一次成功往返的响应快照。
type Response struct {
	// Status 是 HTTP 状态码。
	Status int
	// Header 是响应头（已克隆，可安全持有）。
	Header http.Header
	// Body 是完整响应体（上限 10MB）。
	Body []byte
	// URL 是最终请求的 URL（跟随重定向之后）。
	URL string
}

// Client 是并发安全的 HTTP 客户端，携带限速、robots 缓存与统计计数。
type Client struct {
	opts Options
	hc   *http.Client
	log  *slog.Logger

	statMu   sync.Mutex
	requests int
	retries  int
	blocked  int

	robotsMu sync.Mutex
	robots   map[string]*robotsEntry
}

// New 按 opts 构造客户端，并补齐所有默认值。
//
// 显式构造 http.Client：Timeout 置 0（单次超时由 ctx 控制），Transport 由
// http.DefaultTransport 克隆后放宽连接复用参数，CheckRedirect 限制最多 5 跳。
func New(opts Options) *Client {
	if opts.Timeout <= 0 {
		opts.Timeout = defaultTimeout
	}
	if opts.RobotsTTL <= 0 {
		opts.RobotsTTL = defaultRobotsTTL
	}
	if opts.UserAgent == "" {
		opts.UserAgent = defaultUA
	}
	if opts.Retries < 0 {
		opts.Retries = 0
	}
	logger := opts.Log
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 100
	transport.MaxIdleConnsPerHost = 8
	transport.IdleConnTimeout = 90 * time.Second

	c := &Client{
		opts:   opts,
		log:    logger,
		robots: make(map[string]*robotsEntry),
	}
	c.hc = &http.Client{
		Transport: transport,
		Timeout:   0,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return errTooManyRedirects
			}
			return nil
		},
	}

	logger.Info("初始化 HTTP 客户端",
		"platform", opts.Platform,
		"ua", opts.UserAgent,
		"timeout", opts.Timeout.String(),
		"retries", opts.Retries,
		"respect_robots", opts.RespectRobots,
		"cookie", maskCookie(opts.Cookie),
	)
	return c
}

// GetRaw 发起 GET 请求并返回原始响应体。
//
// 非 2xx 时仍返回响应（便于排查错误页），同时返回 *errs.PlatformError。
func (c *Client) GetRaw(ctx context.Context, rawURL string) ([]byte, *Response, error) {
	resp, err := c.do(ctx, rawURL)
	if err != nil {
		if resp != nil {
			return resp.Body, resp, err
		}
		return nil, resp, err
	}
	return resp.Body, resp, nil
}

// GetText 发起 GET 请求并按字符串返回响应体。
func (c *Client) GetText(ctx context.Context, rawURL string) (string, *Response, error) {
	body, resp, err := c.GetRaw(ctx, rawURL)
	if err != nil {
		return "", resp, err
	}
	return string(body), resp, nil
}

// GetJSON 发起 GET 请求并把响应体解析为 JSON 对象。
//
// 解析失败返回 errs.CodeParseFailed，错误消息附带响应体前 200 字节，
// 便于定位「返回了 HTML 登录页」这类问题。
func (c *Client) GetJSON(ctx context.Context, rawURL string) (map[string]any, *Response, error) {
	body, resp, err := c.GetRaw(ctx, rawURL)
	if err != nil {
		return nil, resp, err
	}

	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, resp, errs.Wrap(errs.CodeParseFailed, err,
			"解析 JSON 失败: %s（响应前 %d 字节: %s）", rawURL, snippetBytes, bodySnippet(body))
	}
	if out == nil {
		out = map[string]any{}
	}
	return out, resp, nil
}

// Stats 返回累计的业务请求数、重试次数与被 robots 拦截的次数。
//
// requests 统计实际发出的业务请求（包含重试），不含 robots.txt 的拉取。
func (c *Client) Stats() (requests, retries, blocked int) {
	c.statMu.Lock()
	defer c.statMu.Unlock()
	return c.requests, c.retries, c.blocked
}

// do 是「robots 网关 + 限速 + 重试」的主流程。
func (c *Client) do(ctx context.Context, rawURL string) (*Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := c.gate(ctx, rawURL); err != nil {
		return nil, err
	}

	total := c.opts.Retries + 1
	var lastErr error
	for attempt := 0; attempt < total; attempt++ {
		if attempt > 0 {
			backoff := retryBackoff(attempt - 1)
			c.incRetries()
			c.log.Warn("请求失败，准备重试",
				"platform", c.opts.Platform,
				"url", rawURL,
				"attempt", attempt,
				"backoff", backoff.String(),
				"err", errText(lastErr),
			)
			if err := sleepCtx(ctx, backoff); err != nil {
				return nil, err
			}
		}

		if c.opts.Limiter != nil {
			if err := c.opts.Limiter.Wait(ctx); err != nil {
				return nil, err
			}
		}

		resp, err := c.doOnce(ctx, rawURL, attempt)
		if err != nil {
			lastErr = err
			if !errs.IsRetryable(err) {
				return nil, err
			}
			continue
		}

		c.incRequests()
		if resp.Status >= 200 && resp.Status < 300 {
			return resp, nil
		}

		perr := statusError(resp, c.opts.Platform)
		lastErr = perr
		if !errs.IsRetryable(perr) {
			return resp, perr
		}
	}

	if lastErr == nil {
		lastErr = errs.New(errs.CodeUnknown, "请求未产生结果: %s", rawURL)
	}
	return nil, lastErr
}

// doOnce 执行一次完整的 HTTP 往返（含 ctx 超时包裹），不处理重试。
func (c *Client) doOnce(ctx context.Context, rawURL string, attempt int) (*Response, error) {
	rctx, cancel := context.WithTimeout(ctx, c.opts.Timeout)
	defer cancel()

	req, err := c.newRequest(rctx, rawURL)
	if err != nil {
		return nil, errs.Wrap(errs.CodeBadConfig, err, "构造请求失败: %s", rawURL)
	}

	start := time.Now()
	httpResp, err := c.hc.Do(req)
	if err != nil {
		if errors.Is(err, errTooManyRedirects) {
			return nil, errs.Wrap(errs.CodeUnknown, err, "重定向超过 %d 跳: %s", maxRedirects, rawURL)
		}
		if errors.Is(err, context.DeadlineExceeded) || rctx.Err() == context.DeadlineExceeded {
			return nil, errs.Wrap(errs.CodeTimeout, err, "请求超时(%s): %s", c.opts.Timeout, rawURL)
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, errs.Wrap(errs.CodeNetwork, err, "请求失败: %s", rawURL)
	}
	defer httpResp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(httpResp.Body, maxBodyBytes))
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || rctx.Err() == context.DeadlineExceeded {
			return nil, errs.Wrap(errs.CodeTimeout, err, "读取响应超时(%s): %s", c.opts.Timeout, rawURL)
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, errs.Wrap(errs.CodeNetwork, err, "读取响应失败: %s", rawURL)
	}

	finalURL := rawURL
	if httpResp.Request != nil && httpResp.Request.URL != nil {
		finalURL = httpResp.Request.URL.String()
	}
	c.log.Info("http 请求",
		"platform", c.opts.Platform,
		"url", rawURL,
		"final_url", finalURL,
		"status", httpResp.StatusCode,
		"bytes", len(body),
		"cost", time.Since(start).String(),
		"attempt", attempt+1,
	)

	return &Response{
		Status: httpResp.StatusCode,
		Header: httpResp.Header.Clone(),
		Body:   body,
		URL:    finalURL,
	}, nil
}

// newRequest 构造带默认浏览器头的 GET 请求。
func (c *Client) newRequest(ctx context.Context, rawURL string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	h := req.Header
	h.Set("User-Agent", c.opts.UserAgent)
	h.Set("Accept-Language", "zh-CN,zh;q=0.9")
	h.Set("Accept", "application/json, text/plain, */*")
	if c.opts.Referer != "" {
		h.Set("Referer", c.opts.Referer)
	}
	if c.opts.Cookie != "" {
		h.Set("Cookie", c.opts.Cookie)
	}
	for k, v := range c.opts.Headers {
		if k == "" {
			continue
		}
		h.Set(k, v)
	}
	return req, nil
}

// statusError 把非 2xx 响应归一为 *errs.PlatformError。
func statusError(resp *Response, platform string) *errs.PlatformError {
	code, ok := errs.ClassifyStatus(resp.Status)
	if !ok {
		code = errs.CodeUnknown
	}
	msg := "HTTP " + strconv.Itoa(resp.Status) + ": " + resp.URL
	if sn := bodySnippet(resp.Body); sn != "" {
		msg += "（响应前 " + strconv.Itoa(snippetBytes) + " 字节: " + sn + "）"
	}
	pe := errs.New(code, "%s", msg)
	pe.Status = resp.Status
	pe.Platform = platform
	return pe
}

// retryBackoff 返回第 n 次重试（从 0 开始）的退避时长：800ms*2^n + 0~300ms 抖动。
func retryBackoff(n int) time.Duration {
	if n < 0 {
		n = 0
	}
	if n > 10 {
		n = 10
	}
	base := 800 * time.Millisecond * time.Duration(int64(1)<<uint(n))
	jitter := time.Duration(rand.Int63n(int64(300 * time.Millisecond)))
	return base + jitter
}

// sleepCtx 是可被 ctx 打断的睡眠。
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// maskCookie 把 cookie 脱敏为「长度 + 首尾各 2 字符」，避免整串进日志。
func maskCookie(cookie string) string {
	if cookie == "" {
		return "len=0"
	}
	n := len(cookie)
	if n <= 6 {
		return "len=" + strconv.Itoa(n)
	}
	return "len=" + strconv.Itoa(n) + " " + cookie[:2] + "***" + cookie[n-2:]
}

// bodySnippet 返回可安全写入日志/错误的响应体前缀。
func bodySnippet(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	if len(body) > snippetBytes {
		body = body[:snippetBytes]
	}
	// 截断点可能切断多字节字符，回退到合法的 rune 边界。
	for len(body) > 0 && !utf8.Valid(body) {
		body = body[:len(body)-1]
	}
	s := string(body)
	// 压缩换行，避免日志被响应体撕成多行。
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r == '\n' || r == '\r' || r == '\t' {
			r = ' '
		}
		out = append(out, r)
	}
	return string(out)
}

// errText 安全提取错误文本，nil 时返回空串。
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// incRequests 累加业务请求数。
func (c *Client) incRequests() {
	c.statMu.Lock()
	c.requests++
	c.statMu.Unlock()
}

// incRetries 累加重试次数。
func (c *Client) incRetries() {
	c.statMu.Lock()
	c.retries++
	c.statMu.Unlock()
}

// incBlocked 累加被 robots 命中/拦截的次数。
func (c *Client) incBlocked() {
	c.statMu.Lock()
	c.blocked++
	c.statMu.Unlock()
}
