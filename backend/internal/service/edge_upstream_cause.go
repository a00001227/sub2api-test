package service

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// EdgeUpstreamCauseHeader 是 cell 在压平上游错误(mapUpstreamError 把 5xx/传输错误
// 统一成 "temporarily unavailable")之前,顺带回给中央的一条**脱敏**错误分类边信道。
//
// 背景:转发请求在中央不走 gateway handler(EdgeForward 反代后 c.Abort()),中央的
// ops 记录由 OpsErrorLoggerMiddleware 读「最终写出去的响应」判定;而 cell relay 过来的
// 5xx body 已被压平成同一句笼统话,中央再聪明也只能解析出这一句。此头把 cell 已知的
// 真实分类带到中央,中央剥掉(不下发客户端)后写入 ops context,复用既有中间件落库。
//
// 值格式: "<upstreamStatus>|<slug>"。**绝不含上游原始文案**(文案可能带账号/请求痕迹);
// 原始文案继续只留在 cell 本地 ops 记录,与现状一致。
const EdgeUpstreamCauseHeader = "X-Sub2api-Upstream-Cause"

// 上游错误分类 slug(与运营页展示一一对应)。
const (
	UpstreamCauseOverloaded        = "overloaded"          // 上游过载
	UpstreamCauseModelNotSupported = "model_not_supported" // 上游模型不支持(如 gpt-5.4/Codex)
	UpstreamCauseClientVersionGate = "client_version_gate" // 客户端版本门槛(如 fable 要求新版 CLI)
	UpstreamCauseProxyDown         = "proxy_down"          // 传输层错误(无 HTTP 响应,疑似代理出口异常)
	UpstreamCauseOther5xx          = "other_5xx"           // 其它 5xx 上游错误(兜底)
)

// ClassifyUpstreamCause 按(上游状态码, 上游原始文案)归一出一个错误分类 slug。
// 仅用于把被 mapUpstreamError 压平的 5xx / 传输错误细分;无法归类时返回 ""(不打标)。
//
// 判定优先级:先按文案关键词命中细类(client_version_gate 必须排在 model_not_supported
// 之前——二者都可能含 "support"),命中不了再按状态码兜底。4xx 且文案未命中已知细类时
// 返回 ""(交由既有处理,避免把正常 4xx 归成一坨)。
func ClassifyUpstreamCause(upstreamStatus int, upstreamMsg string) string {
	m := strings.ToLower(upstreamMsg)
	switch {
	case strings.Contains(m, "does not support this model") &&
		(strings.Contains(m, "version") || strings.Contains(m, "claude update")):
		return UpstreamCauseClientVersionGate
	case strings.Contains(m, "not supported"):
		return UpstreamCauseModelNotSupported
	case strings.Contains(m, "overloaded"):
		return UpstreamCauseOverloaded
	}
	switch {
	case upstreamStatus <= 0:
		return UpstreamCauseProxyDown
	case upstreamStatus == 529:
		return UpstreamCauseOverloaded
	case upstreamStatus >= 500:
		return UpstreamCauseOther5xx
	default:
		return ""
	}
}

// SetEdgeUpstreamCauseHeader 在 cell 压平错误前,把分类写进脱敏响应头(供中央剥取)。
// 仅在响应尚未开始回写(!streamStarted)时可设——流已开始则响应头已 flush,跳过。
func SetEdgeUpstreamCauseHeader(c *gin.Context, streamStarted bool, upstreamStatus int, upstreamMsg string) {
	if c == nil || streamStarted {
		return
	}
	slug := ClassifyUpstreamCause(upstreamStatus, upstreamMsg)
	if slug == "" {
		return
	}
	c.Header(EdgeUpstreamCauseHeader, fmt.Sprintf("%d|%s", upstreamStatus, slug))
}

// ParseEdgeUpstreamCauseHeader 解析 "<status>|<slug>"。中央 EdgeForward 用;
// 空/格式非法返回 ok=false。status 缺失或非数字时置 0(仍返回 slug)。
func ParseEdgeUpstreamCauseHeader(v string) (status int, slug string, ok bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, "", false
	}
	parts := strings.SplitN(v, "|", 2)
	if len(parts) != 2 {
		return 0, "", false
	}
	slug = strings.TrimSpace(parts[1])
	if slug == "" {
		return 0, "", false
	}
	status, _ = strconv.Atoi(strings.TrimSpace(parts[0]))
	return status, slug, true
}
