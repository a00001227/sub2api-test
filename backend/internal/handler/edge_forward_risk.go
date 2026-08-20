package handler

import (
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// TODO(risk-v2 diag): 临时诊断日志，定位转发采集为何不写。定位后移除。
func fwdRiskLog() *zap.Logger {
	return logger.L().With(zap.String("component", "handler.risk_v2.fwd_observe"))
}

// ObserveForwardedRiskV2 在中央转发到 cell 成功后，为 Claude Code 请求补一条 Risk V2 观测。
//
// 转发路径由 EdgeForward 中间件反向代理并 c.Abort()，gateway handler 从不执行 →
// 本地 V2 采集(buildRiskUsageFeatures → enqueueRiskV2)会漏。这里在响应回传后
// (biller 回调，不在延迟热路径)从缓冲的请求体解析出请求侧特征，配合 cell 回传的
// 权威用量入队一条观测。仅观测、best-effort，失败静默；关闭 risk.v2 时零开销。
//
// 范围：只针对 Claude Code(User-Agent claude-cli)——复用现成校验器；其余客户端/平台
// (Codex/Gemini 等)UA 不匹配即跳过，无需平台分支。
func (h *GatewayHandler) ObserveForwardedRiskV2(c *gin.Context, env service.EdgeUsageEnvelope, reqBody []byte, startedAt time.Time) {
	ua := c.GetHeader("User-Agent")
	fwdRiskLog().Info("enter", zap.String("ua", ua), zap.Int("body_len", len(reqBody)), zap.String("platform", env.Platform))
	if h == nil || h.cfg == nil {
		fwdRiskLog().Info("skip", zap.String("reason", "nil_handler_or_cfg"))
		return
	}
	if !h.cfg.Risk.V2.EffectiveEnabled() {
		fwdRiskLog().Info("skip", zap.String("reason", "not_effective_enabled"))
		return
	}
	// 只针对 Claude Code：UA 必须是 claude-cli/x.x.x。
	if !claudeCodeValidator.ValidateUserAgent(ua) {
		fwdRiskLog().Info("skip", zap.String("reason", "ua_not_claude_code"), zap.String("ua", ua))
		return
	}
	apiKey, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok || apiKey == nil {
		fwdRiskLog().Info("skip", zap.String("reason", "no_apikey_in_context"))
		return
	}
	// Claude Code 恒走 Anthropic Messages 协议。
	parsed, err := service.ParseGatewayRequest(service.NewRequestBodyRef(reqBody), domain.PlatformAnthropic)
	if err != nil || parsed == nil {
		fwdRiskLog().Info("skip", zap.String("reason", "parse_failed"), zap.Error(err), zap.Int("body_len", len(reqBody)))
		return
	}
	feat := buildRiskUsageFeatures(parsed, h.cfg, startedAt, newRiskV2ServerRequestID(c))
	if feat.V2Request == nil {
		fwdRiskLog().Info("skip", zap.String("reason", "nil_v2request"))
		return
	}
	h.gatewayService.EnqueueForwardedRiskV2(apiKey, env, feat)
	fwdRiskLog().Info("enqueued", zap.Int64("user_id", apiKey.UserID), zap.Int64("api_key_id", apiKey.ID), zap.String("model", env.Model))
}
