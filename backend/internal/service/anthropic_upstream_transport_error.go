package service

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
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
//   - 读/响应侧超时（read/header timeout 600~900s 才触发）此时 CF 早已切断客户端，客户端已收到
//     错误或已放弃；此时换号会对上游**重复执行**同一请求，且新请求同样超不出 CF 窗口 —— 纯亏。
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

// isConnectPhaseTransportError 判断传输层错误是否发生在"连接建立阶段"——dial 超时、
// 拒连、主机/网络不可达、DNS 解析失败、TLS 握手失败。这些失败上游都未收到请求，
// 换号安全（不双执行）且快（≤dial_timeout）。
//
// 对读/响应侧错误（读超时、EOF、响应中途连接被重置）返回 false：它们只在 600~900s 的
// 读/头超时后才出现，此时 CF 已切断客户端，换号只会对上游重复执行。
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
	safeErr := sanitizeUpstreamErrorMessage(err.Error())
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

	if isConnectPhaseTransportError(err) && upstreamFailoverWithinBudget(c) {
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
