package service

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
)

// anthropic_upstream_transport_error.go —— Anthropic 网关传输层失败（DoWithTLS 返回
// err、没有 HTTP 响应：代理/DNS/TCP/TLS 出错）的统一处理。
//
// 与 OpenAI 路径（handleOpenAIUpstreamTransportError，对所有非取消错误都 failover）不同，
// 这里**只对"连接建立阶段"的失败换号**，且受一个请求级时间预算约束。原因是 Cloudflare
// Tunnel 的会话超时是 100s：
//   - 连接建立失败（dial 超时 / 拒连 / DNS / 网络不可达 / TLS 握手）在 dial_timeout（~10s）
//     内就返回，上游根本没收到请求，换号既安全（不双执行）又快，重试仍来得及赶在 CF 掐断前完成。
//   - 响应头之前的连接被掐（EOF / connection reset by peer）——死号被 Anthropic 边缘/WAF
//     立刻 RST、代理断连——也在此换号：本分类器只在 DoWithTLS 返回 err 的三处站点被调用，
//     此刻响应头尚未到达（响应中途的重置/读超时不会走到这里，见下），故这类 EOF 意味着上游
//     没有回答本次请求，换号安全（不双执行已生成的回答）且快，仍受 upstreamFailoverBudget 约束。
//   - 读/响应侧超时（read/header timeout 600~900s 才触发）此时 CF 早已切断客户端，客户端已收到
//     错误或已放弃；此时换号会对上游**重复执行**同一请求，且新请求同样超不出 CF 窗口 —— 纯亏。
//     这类慢超时表现为 context.DeadlineExceeded，仍返回 false；响应中途的连接重置发生在读 body
//     阶段（SSE scanner），根本到不了本分类器。
//   - 客户端主动断开（context.Canceled）：上游没机会暴露故障，不换号。
//
// 不做账号驱逐（不同于 OpenAI 的 tempUnschedule 副作用）：连接级失败多是瞬时网络抖动，
// 直接换到健康账号即可，坏账号/坏代理的持续性剔除交由既有 403/5xx 路径与健康检查处理。

// upstreamFailoverBudget 限制"连接级传输失败"最多累计换号多久。每次 dial 失败在
// dial_timeout（~10s）内返回，账号切换上限 maxAccountSwitches（默认 10）→ 最坏 ~100s，
// 恰好撞上 Cloudflare Tunnel 的 100s 截断。一旦本请求已耗时超过该预算就不再换号，
// 让当前这次重试还有余量在 CF 掐断前完成。
const upstreamFailoverBudget = 60 * time.Second

// anthropicTransportFailoverBudgetKey 是懒存进 gin.Context 的"首次传输失败时刻"键。
// gin.Context 在同一请求的跨账号 failover 循环中是共享的，故可用它累计请求已耗时。
const anthropicTransportFailoverBudgetKey = "anthropic_upstream_failover_start"

// anthropicTransportFailoverBody 是失败透传给客户端的 Anthropic 格式错误体，与旧的内联
// 502 body 保持一致：若 failover 最终耗尽，客户端看到的载荷不变。
var anthropicTransportFailoverBody = []byte(`{"type":"error","error":{"type":"upstream_error","message":"Upstream request failed"}}`)

// isConnectPhaseTransportError 判断传输层错误是否属于"上游未回答本次请求、换号安全"的一类——
// dial 超时、拒连、主机/网络不可达、DNS 解析失败、TLS 握手失败，以及**响应头之前**连接被
// 对端掐断（EOF / connection reset by peer，典型是死号被 Anthropic 边缘/WAF 立刻 RST）。
// 这些失败上游都没有回答本次请求，换号既安全（不双执行）又快。
//
// 前提不变式：本函数只在 DoWithTLS 返回 err（尚无 HTTP 响应头）的站点被调用，所以这里的 EOF/RST
// 必然发生在响应头之前；响应中途的重置/读 body 错误在 SSE scanner 里单独处理，不会到这里。
// 读/头侧的慢超时表现为 context.DeadlineExceeded，仍返回 false（保持原行为写 502，避免对上游
// 重复执行同一已处理请求）。
func isConnectPhaseTransportError(err error) bool {
	if err == nil {
		return false
	}
	// 客户端主动断开：永远不是连接建立阶段失败。
	if errors.Is(err, context.Canceled) {
		return false
	}
	// dial 阶段的 net.OpError（Op == "dial"）：连接从未建立，即便底层是超时也算连接级。
	// 放在 DeadlineExceeded 判断之前，确保 dial 超时被正确归为连接级。
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return true
	}
	// 拒连 / 主机不可达 / 网络不可达。
	if errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.EHOSTUNREACH) ||
		errors.Is(err, syscall.ENETUNREACH) {
		return true
	}
	// DNS 解析失败（坏/过期的代理主机名、no such host）。
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	// TLS 握手记录层错误（在任何 HTTP 响应之前）。
	var recordErr tls.RecordHeaderError
	if errors.As(err, &recordErr) {
		return true
	}
	// 响应头之前连接被对端关闭/重置：EOF（`Post "...": EOF`，Go http client 在拿到响应头前
	// 连接被关时返回，errors.Is 会穿透 *url.Error 命中 io.EOF）、半截 EOF、connection reset by peer。
	// 依据上面的不变式，这里必然是响应头之前，上游未回答本次请求 → 换号安全。
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET) {
		return true
	}
	// 读/响应侧的超时（非 dial）：不算连接级，保持原行为写 502。
	if errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	// 无类型形态的错误（如 golang.org/x/net/proxy 对 SOCKS 拨号拒绝返回的纯字符串）
	// 用字符串标记兜底。刻意只匹配连接建立类信号。
	msg := strings.ToLower(err.Error())
	for _, marker := range []string{
		"connection refused",
		"no route to host",
		"network is unreachable",
		"no such host",
		"tls: handshake",
		"tls handshake",
		"connection reset by peer",
		"unexpected eof",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// upstreamFailoverWithinBudget 报告本请求在"连接级传输失败换号"预算内是否还有余量。
// 首次调用（本请求第一次传输失败）记录起点并放行；之后按 gin.Context 里累计的已耗时判断。
// gin.Context 在同一请求跨账号 failover 循环中共享，故能跨账号累计。
func upstreamFailoverWithinBudget(c *gin.Context) bool {
	if c == nil {
		return true
	}
	if v, ok := c.Get(anthropicTransportFailoverBudgetKey); ok {
		if start, ok := v.(time.Time); ok {
			return time.Since(start) < upstreamFailoverBudget
		}
	}
	c.Set(anthropicTransportFailoverBudgetKey, time.Now())
	return true
}

// handleAnthropicUpstreamTransportError 统一处理 Anthropic 网关的传输层失败：
//  1. 记录 Ops 错误日志（status 0，kind=request_error）；
//  2. 若是"连接建立阶段"失败且仍在换号时间预算内，返回 *UpstreamFailoverError（交由
//     handler 换到健康账号，本函数**不写响应**——响应由 handler 拥有）；
//  3. 否则（读侧超时 / 客户端断开 / 预算耗尽）保持原行为：直接写 502，本函数拥有响应。
//
// passthrough 为透传分支的 Ops 事件打标（对齐原三处站点：透传分支置 true，其余 false）。
func handleAnthropicUpstreamTransportError(c *gin.Context, account *Account, upstreamReq *http.Request, err error, passthrough bool) error {
	safeErr, failover := recordAnthropicTransportFailover(c, account, upstreamReq, err, passthrough)
	if failover {
		// 不写响应：由 handler 换号，或换号耗尽后写协议正确的错误。
		return &UpstreamFailoverError{
			StatusCode:   http.StatusBadGateway,
			ResponseBody: anthropicTransportFailoverBody,
		}
	}

	// 读侧超时 / 客户端断开 / 预算耗尽：保持原行为，直接写 502。
	c.JSON(http.StatusBadGateway, gin.H{
		"type": "error",
		"error": gin.H{
			"type":    "upstream_error",
			"message": "Upstream request failed",
		},
	})
	return fmt.Errorf("upstream request failed: %s", safeErr)
}

// recordAnthropicTransportFailover 记录 Anthropic 网关传输层失败的 Ops 事件，并判定本次
// 请求是否应换号。返回 failover=true 时，调用方**必须**返回 *UpstreamFailoverError 且
// **不写响应**（响应由 handler 拥有）；failover=false 时，调用方自行写协议正确的 502。
//
// 抽出此函数是为了让 CC(/v1/chat/completions) 与 Responses(/v1/responses) 两条转发路径复用
// 与主 /v1/messages 路径完全一致的分类与 Ops 记录，只在最终兜底 502 的响应格式上各自处理。
func recordAnthropicTransportFailover(c *gin.Context, account *Account, upstreamReq *http.Request, err error, passthrough bool) (safeErr string, failover bool) {
	safeErr = sanitizeUpstreamErrorMessage(err.Error())
	setOpsUpstreamError(c, 0, safeErr, "")
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Platform:           account.Platform,
		AccountID:          account.ID,
		AccountName:        account.Name,
		UpstreamStatusCode: 0,
		UpstreamURL:        safeUpstreamURL(upstreamReq.URL.String()),
		Passthrough:        passthrough,
		Kind:               "request_error",
		Message:            safeErr,
	})
	return safeErr, isConnectPhaseTransportError(err) && upstreamFailoverWithinBudget(c)
}
