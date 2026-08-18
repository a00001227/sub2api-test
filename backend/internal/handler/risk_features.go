package handler

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/Wei-Shaw/sub2api/internal/config"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// riskV2ObservedCtxKey 是 gin 上下文标记：成功记账路径已提交 V2 观测。
// 终态 defer 据此避免对成功请求再产生一条终态观测（dispatcher 另有 server_request_id 去重兜底）。
const riskV2ObservedCtxKey = "risk_v2_observed"

// riskV2ServerReqIDCtxKey 存放本次下游请求的 server_request_id（平台入口生成，每请求唯一）。
const riskV2ServerReqIDCtxKey = "risk_v2_server_request_id"

// newRiskV2ServerRequestID 在下游请求入口生成 server_request_id 并存入 gin 上下文。
// 平台服务端生成、每请求唯一；绝不来自客户端 Header/请求体/billing/idempotency/客户端自定义 ID。
// 同一次请求的成功记账点与终态 defer 共用同一个 ID（内部多次回调/上游重试不产生额外观测）。
func newRiskV2ServerRequestID(c *gin.Context) string {
	id := uuid.NewString()
	if c != nil {
		c.Set(riskV2ServerReqIDCtxKey, id)
	}
	return id
}

func riskV2ServerRequestID(c *gin.Context) string {
	if c == nil {
		return ""
	}
	if v, ok := c.Get(riskV2ServerReqIDCtxKey); ok {
		if id, _ := v.(string); id != "" {
			return id
		}
	}
	return ""
}

// markRiskV2Observed 在成功记账点标记本请求已产生（或将异步产生）一条 V2 观测。
func markRiskV2Observed(c *gin.Context) {
	if c != nil {
		c.Set(riskV2ObservedCtxKey, true)
	}
}

// riskV2TerminalObserve 作为各 gateway handler 入口的 defer：当请求未走到成功记账路径
// （上游错误/取消/流中断/panic）时，产生一条 usage_available=false 的终态观测。
// 成功路径（已 markRiskV2Observed）跳过。身份不足（未过鉴权）跳过。去重键为 server_request_id。
func (h *GatewayHandler) riskV2TerminalObserve(c *gin.Context, startedAt time.Time) {
	if h == nil || h.cfg == nil || !h.cfg.Risk.V2.EffectiveEnabled() {
		return
	}
	if v, ok := c.Get(riskV2ObservedCtxKey); ok {
		if done, _ := v.(bool); done {
			return
		}
	}
	apiKey, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok || apiKey == nil {
		return
	}
	terminal := "terminal_error"
	if status := c.Writer.Status(); status > 0 {
		// 只保留状态码数字，绝不含任何请求内容。
		terminal = "terminal_status_" + strconv.Itoa(status)
	}
	h.gatewayService.EnqueueRiskV2Terminal(riskV2ServerRequestID(c), apiKey.UserID, apiKey.ID, startedAt, terminal)
}

// riskSimhashCaptureBytes 限制从消息 raw 拷贝的字节数（首 8KB）。simhash 归一化
// 内部还会再截断，这里先限拷贝量以免大请求体在异步闭包里长时间保活。
const riskSimhashCaptureBytes = 8 * 1024

// buildRiskUsageFeatures 从已解析请求提取 Risk Phase 0（仅观测）请求特征。
// 必须在 parsedReq 仍有效、请求体未被复用时调用，并把结果作为标量/独立副本捕获进
// 响应后闭包——绝不在异步闭包里持有 parsedReq/请求体引用。
// 隐私：只取计数 + 消息 raw 的截断副本（供异步计算 64 位 simhash），绝不落原文。
// cfg 可为 nil；仅当 cfg.Risk.V2.Enabled 时额外计算 V2 请求侧特征（哈希/标量，无原文）。
func buildRiskUsageFeatures(parsed *service.ParsedRequest, cfg *config.Config, startedAt time.Time, serverReqID string) service.RiskUsageFeatures {
	if parsed == nil {
		return service.RiskUsageFeatures{RequestStartedAt: startedAt, ServerRequestID: serverReqID}
	}
	feat := service.RiskUsageFeatures{
		MessageCount:     parsed.MessageCount(),
		MaxTokens:        parsed.MaxTokens,
		Temperature:      parsed.Temperature,
		RequestStartedAt: startedAt,
		ServerRequestID:  serverReqID,
	}
	if raw := parsed.MessagesRaw(); len(raw) > 0 {
		n := len(raw)
		if n > riskSimhashCaptureBytes {
			n = riskSimhashCaptureBytes
		}
		// 独立副本：parsedReq 的 raw 是零拷贝引用底层请求体，闭包异步执行时该缓冲可能已被复用。
		cp := make([]byte, n)
		copy(cp, raw[:n])
		feat.MessagesRaw = cp
	}
	// Model Extraction Risk V2 Shadow（P1A）：flag 关时 BuildRiskV2RequestFeatures 返回 nil，
	// V2Request 保持 nil，采集完全不发生（legacy 行为不变）。
	if cfg != nil {
		feat.V2Request = service.BuildRiskV2RequestFeatures(parsed, cfg.Risk.V2)
	}
	return feat
}
