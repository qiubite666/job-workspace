package httpx

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"jobworkspace/internal/errs"
)

const (
	// robotsFailureTTL 是 robots.txt 拉取失败后的重缓存时长（短一些，便于自愈）。
	robotsFailureTTL = 10 * time.Minute
	// maxRobotsBytes 是 robots.txt 的最大读取长度。
	maxRobotsBytes = 512 << 10
)

// robotsEntry 是单个 origin 的 robots.txt 解析结果缓存。
type robotsEntry struct {
	rules   []string  // 选中 User-agent 段下的 Disallow 规则原文
	fetched time.Time // 拉取时刻
	ttl     time.Duration
	ok      bool // false 表示拉取失败，按「未声明限制」处理
}

// CheckRobots 检查 rawURL 是否被 robots.txt 允许。
//
// allowed=false 时 reason 为命中的规则原文（如 "/search/*"）。err 仅在 URL 非法
// 或 ctx 取消时返回；robots.txt 拉取失败（404 / 网络错误 / 超时）一律视为
// 「未声明限制」放行，不阻断采集。
//
// 本方法只是查询，不改变 Stats 的 blocked 计数：计数发生在真正发请求的网关上。
func (c *Client) CheckRobots(ctx context.Context, rawURL string) (allowed bool, reason string, err error) {
	return c.checkRobots(ctx, rawURL)
}

// checkRobots 是 CheckRobots 的内部实现。
func (c *Client) checkRobots(ctx context.Context, rawURL string) (bool, string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if !c.opts.RespectRobots {
		return true, "未启用 robots 检查", nil
	}

	u, err := url.Parse(rawURL)
	if err != nil {
		return false, "", errs.Wrap(errs.CodeBadConfig, err, "解析 URL 失败: %s", rawURL)
	}
	if u.Scheme == "" || u.Host == "" {
		return false, "", errs.New(errs.CodeBadConfig, "URL 缺少 scheme 或 host: %s", rawURL)
	}
	origin := u.Scheme + "://" + u.Host

	entry, err := c.robotsFor(ctx, origin)
	if err != nil {
		// 主动取消（含超时）要上报；其余一律按未声明限制放行。
		if ctxErr := ctx.Err(); ctxErr != nil {
			return false, "", ctxErr
		}
		c.log.Warn("拉取 robots.txt 失败，按未声明限制放行",
			"platform", c.opts.Platform, "origin", origin, "err", errText(err))
		return true, "", nil
	}
	if entry == nil || !entry.ok || len(entry.rules) == 0 {
		return true, "", nil
	}

	target := u.RequestURI()
	for _, rule := range entry.rules {
		if matchRobotsPattern(rule, target) {
			return false, rule, nil
		}
	}
	return true, "", nil
}

// gate 是请求前的 robots 网关：命中规则时按配置拦截或告警放行。
func (c *Client) gate(ctx context.Context, rawURL string) error {
	allowed, rule, err := c.checkRobots(ctx, rawURL)
	if err != nil {
		return err
	}
	if allowed {
		return nil
	}
	if c.opts.AllowDisallowed {
		c.incBlocked()
		c.log.Warn("robots.txt 禁止抓取，但已授权放行",
			"platform", c.opts.Platform, "url", rawURL, "rule", rule)
		return nil
	}
	c.incBlocked()
	return errs.New(errs.CodeRobotsDisallowed,
		"robots.txt 禁止抓取 %s（命中规则: %s）", rawURL, rule)
}

// robotsFor 返回 origin 的 robots 解析结果，命中缓存时不再发请求。
func (c *Client) robotsFor(ctx context.Context, origin string) (*robotsEntry, error) {
	now := time.Now()

	c.robotsMu.Lock()
	if e, ok := c.robots[origin]; ok && now.Sub(e.fetched) < e.ttl {
		c.robotsMu.Unlock()
		return e, nil
	}
	c.robotsMu.Unlock()

	entry, err := c.fetchRobots(ctx, origin)
	if err != nil {
		// 失败也进缓存（短 TTL），避免每个请求都重试 robots。
		c.robotsMu.Lock()
		c.robots[origin] = &robotsEntry{fetched: now, ttl: robotsFailureTTL}
		c.robotsMu.Unlock()
		return nil, err
	}

	entry.ttl = c.opts.RobotsTTL
	c.robotsMu.Lock()
	c.robots[origin] = entry
	c.robotsMu.Unlock()
	return entry, nil
}

// fetchRobots 拉取并解析 <origin>/robots.txt。
//
// 该请求不走 robots 网关（否则递归），也不消耗重试预算；但同样经过限速器。
func (c *Client) fetchRobots(ctx context.Context, origin string) (*robotsEntry, error) {
	robotsURL := origin + "/robots.txt"

	rctx, cancel := context.WithTimeout(ctx, c.opts.Timeout)
	defer cancel()

	if c.opts.Limiter != nil {
		if err := c.opts.Limiter.Wait(rctx); err != nil {
			return nil, err
		}
	}

	req, err := c.newRequest(rctx, robotsURL)
	if err != nil {
		return nil, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	entry := &robotsEntry{fetched: time.Now(), ok: true}
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		// 没有 robots.txt 即未声明限制，放行全部。
		return entry, nil
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("robots.txt 返回 HTTP %d", resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxRobotsBytes))
	if err != nil {
		return nil, err
	}
	entry.rules = parseRobotsRules(string(data), c.opts.UserAgent)
	return entry, nil
}

// parseRobotsRules 从 robots.txt 中取出适配当前 UA 的 Disallow 规则。
//
// 选择顺序：与当前 UA 前缀/包含匹配的段（取匹配串最长者）> User-agent: * 段。
// 空 Disallow 视为「放行全部」，直接跳过。
func parseRobotsRules(text, userAgent string) []string {
	type group struct {
		agents []string
		rules  []string
	}

	var groups []group
	cur := -1
	for _, line := range strings.Split(text, "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" {
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		val = strings.TrimSpace(val)

		switch key {
		case "user-agent":
			// 规则之后出现的 User-agent 开启新段。
			if cur < 0 || len(groups[cur].rules) > 0 {
				groups = append(groups, group{})
				cur = len(groups) - 1
			}
			if val != "" {
				groups[cur].agents = append(groups[cur].agents, strings.ToLower(val))
			}
		case "disallow":
			if cur < 0 {
				groups = append(groups, group{})
				cur = 0
			}
			groups[cur].rules = append(groups[cur].rules, val)
		}
	}

	ua := strings.ToLower(userAgent)
	bestIdx, bestLen, wildcardIdx := -1, -1, -1
	for i, g := range groups {
		for _, a := range g.agents {
			if a == "*" {
				if wildcardIdx < 0 {
					wildcardIdx = i
				}
				continue
			}
			if len(a) > bestLen && uaContainsUA(ua, a) {
				bestIdx, bestLen = i, len(a)
			}
		}
	}

	idx := bestIdx
	if idx < 0 {
		idx = wildcardIdx
	}
	if idx < 0 {
		return nil
	}

	rules := make([]string, 0, len(groups[idx].rules))
	for _, r := range groups[idx].rules {
		if r == "" {
			continue // 空 Disallow 表示放行全部
		}
		rules = append(rules, r)
	}
	return rules
}

// uaContainsUA 判断当前 UA 是否命中某条 robots 里的 User-agent 声明。
//
// 声明允许是完整 UA 前缀，也允许是产品名（如 "Googlebot"、"boss"）。
func uaContainsUA(ua, agent string) bool {
	if agent == "" {
		return false
	}
	return strings.Contains(ua, agent)
}

// matchRobotsPattern 判断 URL 路径是否命中单条 robots 规则。
//
// 规则语义与 RFC 9309 一致：
//   - 无通配符时按前缀匹配（"/admin" 命中 "/administrator"）；
//   - "*" 匹配任意长度字符串，可出现在首尾与中间；
//   - 尾部 "$" 表示必须精确到路径结尾（"/search$" 不命中 "/search?q=1"）。
func matchRobotsPattern(pattern, path string) bool {
	if pattern == "" {
		return false
	}

	anchored := strings.HasSuffix(pattern, "$")
	if anchored {
		pattern = pattern[:len(pattern)-1]
	}
	if pattern == "" {
		return anchored && path == ""
	}

	parts := strings.Split(pattern, "*")
	pos := 0
	for i, part := range parts {
		if part == "" {
			continue
		}
		if i == 0 && !strings.HasPrefix(pattern, "*") {
			if !strings.HasPrefix(path, part) {
				return false
			}
			pos = len(part)
			continue
		}
		idx := strings.Index(path[pos:], part)
		if idx < 0 {
			return false
		}
		pos += idx + len(part)
	}

	if anchored {
		return pos == len(path)
	}
	return true
}
