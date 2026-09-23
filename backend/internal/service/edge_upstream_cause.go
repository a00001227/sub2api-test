package service

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"unicode"

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

// EdgeUpstreamDetailHeader 是 cell 回给中央的第二条边信道:上游错误的**脱敏摘要**
// (状态码 + 脱敏后的上游文案 + cell 上的账号名/平台 + 上游 request id)。
//
// 与 EdgeUpstreamCauseHeader 的分工:Cause 只带规范 slug,供 SLA/健康分分类,永不变;
// Detail 带"人看的"信息,供运维弹窗直接看到上游原话和是哪个号,免去逐台 cell 翻日志。
// 文案已经过 sanitizeUpstreamErrorMessage + 控制字符剥离 + 限长;值为 url.QueryEscape 后的
// JSON,保证纯 ASCII 可进响应头。中央剥掉不下发客户端;旧版中央不认识该头会原样透传给
// 客户端,故 cell/中央要一起上线(先中央后 cell 更稳)。
const EdgeUpstreamDetailHeader = "X-Sub2api-Upstream-Detail"

// edgeUpstreamDetail 限长:文案 400 字符、账号名 128 字符,整头不超过 ~1.5KB。
const (
	edgeUpstreamDetailMaxMessage = 400
	edgeUpstreamDetailMaxName    = 128
)

// EdgeUpstreamDetail 是 EdgeUpstreamDetailHeader 的载荷。
type EdgeUpstreamDetail struct {
	Status      int    `json:"s,omitempty"`
	Message     string `json:"m,omitempty"`
	Platform    string `json:"p,omitempty"`
	AccountID   int64  `json:"aid,omitempty"`
	AccountName string `json:"an,omitempty"`
	RequestID   string `json:"rid,omitempty"`
	Kind        string `json:"k,omitempty"`
}

// 上游错误分类 slug(与运营页展示一一对应)。
const (
	UpstreamCauseOverloaded        = "overloaded"          // 上游过载
	UpstreamCauseModelNotSupported = "model_not_supported" // 上游模型不支持(如 gpt-5.4/Codex)
	UpstreamCauseClientVersionGate = "client_version_gate" // 客户端版本门槛(如 fable 要求新版 CLI)
	UpstreamCauseProxyDown         = "proxy_down"          // 传输层错误(无 HTTP 响应,疑似代理出口异常)
	UpstreamCauseOther5xx          = "other_5xx"           // 其它 5xx 上游错误(兜底)
	UpstreamCauseRequestTooLarge   = "request_too_large"   // 上游 413:请求体超上限(客户端上下文过大,非中转的锅)
	UpstreamCauseClientCanceled    = "client_canceled"     // 传输层 context canceled:客户端中途断开(非中转的锅,区别于 proxy_down)
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
	case strings.Contains(m, "request_too_large") || strings.Contains(m, "exceeds the maximum size"):
		return UpstreamCauseRequestTooLarge
	case strings.Contains(m, "an error occurred while processing your request") && strings.Contains(m, "help.openai.com"):
		// OpenAI 网关级内部错误的固定模板(server_error:"…contact us through our help center at
		// help.openai.com… include the request ID …")。常以 HTTP 200 流终态 response.failed 出现,
		// 拿不到 5xx 状态码,只能按文案认;性质等同上游 5xx → 归 other_5xx,不计中转错误。
		return UpstreamCauseOther5xx
	}
	switch {
	case upstreamStatus <= 0:
		// 传输层无 HTTP 响应。context canceled = 客户端中途断开(非中转的锅);
		// 与真正的出口拨号/连接失败(proxy_down,计入中转 SLA)区分开。
		if strings.Contains(m, "context canceled") || strings.Contains(m, "context cancelled") {
			return UpstreamCauseClientCanceled
		}
		return UpstreamCauseProxyDown
	case upstreamStatus == 413:
		return UpstreamCauseRequestTooLarge
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
	if c == nil {
		return
	}
	slug := ClassifyUpstreamCause(upstreamStatus, upstreamMsg)
	if slug != "" {
		// 先把权威 slug 落进 cell 自己的 ops context(与 streamStarted 无关:算 slug 不需要发头)——
		// cell 落自己那条 ops 行时据此分类,SLA 排除口径与中央 edge 行统一。
		c.Set(OpsUpstreamCauseSlugKey, slug)
	}
	// 响应头只在流未开始回写时能加(已开始则头已 flush);带回中央供其 edge 行分类。
	if streamStarted {
		return
	}
	if slug != "" {
		c.Header(EdgeUpstreamCauseHeader, fmt.Sprintf("%d|%s", upstreamStatus, slug))
	}
	// 摘要头不依赖 slug:上游 401/403/429 这类没有分类的错误也要让中央看到原话和账号。
	if d := BuildEdgeUpstreamDetail(c, upstreamStatus, upstreamMsg); d != nil {
		c.Header(EdgeUpstreamDetailHeader, EncodeEdgeUpstreamDetail(d))
	}
}

// BuildEdgeUpstreamDetail 组装摘要:显式传入的状态码/文案优先,缺的部分从 cell 自己的
// ops 上游事件列表(最后一条)补齐 —— 账号名/平台/上游 request id 只有事件里有。
// 什么都没有时返回 nil(不发头)。
func BuildEdgeUpstreamDetail(c *gin.Context, upstreamStatus int, upstreamMsg string) *EdgeUpstreamDetail {
	if c == nil {
		return nil
	}
	d := &EdgeUpstreamDetail{
		Status:  upstreamStatus,
		Message: sanitizeEdgeDetailText(upstreamMsg, edgeUpstreamDetailMaxMessage),
	}
	if v, ok := c.Get(OpsUpstreamErrorsKey); ok {
		if events, ok := v.([]*OpsUpstreamErrorEvent); ok && len(events) > 0 {
			if last := events[len(events)-1]; last != nil {
				if d.Status == 0 {
					d.Status = last.UpstreamStatusCode
				}
				if d.Message == "" {
					d.Message = sanitizeEdgeDetailText(last.Message, edgeUpstreamDetailMaxMessage)
				}
				d.Platform = strings.TrimSpace(last.Platform)
				d.AccountID = last.AccountID
				d.AccountName = sanitizeEdgeDetailText(last.AccountName, edgeUpstreamDetailMaxName)
				d.RequestID = sanitizeEdgeDetailText(last.UpstreamRequestID, edgeUpstreamDetailMaxName)
				d.Kind = strings.TrimSpace(last.Kind)
			}
		}
	}
	if d.Status == 0 && d.Message == "" && d.AccountName == "" {
		return nil
	}
	return d
}

// sanitizeEdgeDetailText 脱敏(复用上游文案脱敏)+ 剥控制字符 + 按 rune 限长。
func sanitizeEdgeDetailText(s string, max int) string {
	s = sanitizeUpstreamErrorMessage(strings.TrimSpace(s))
	if s == "" {
		return ""
	}
	var b strings.Builder
	n := 0
	for _, r := range s {
		if unicode.IsControl(r) {
			r = ' '
		}
		if n >= max {
			b.WriteString("…")
			break
		}
		b.WriteRune(r)
		n++
	}
	return strings.TrimSpace(b.String())
}

// EncodeEdgeUpstreamDetail 序列化为纯 ASCII 头值(JSON → QueryEscape)。
func EncodeEdgeUpstreamDetail(d *EdgeUpstreamDetail) string {
	if d == nil {
		return ""
	}
	raw, err := json.Marshal(d)
	if err != nil {
		return ""
	}
	return url.QueryEscape(string(raw))
}

// ParseEdgeUpstreamDetailHeader 反解 EncodeEdgeUpstreamDetail 的头值;空/非法返回 nil。
func ParseEdgeUpstreamDetailHeader(v string) *EdgeUpstreamDetail {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	raw, err := url.QueryUnescape(v)
	if err != nil {
		return nil
	}
	var d EdgeUpstreamDetail
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		return nil
	}
	if d.Status == 0 && d.Message == "" && d.AccountName == "" {
		return nil
	}
	return &d
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
