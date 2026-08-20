package service

import "github.com/Wei-Shaw/sub2api/internal/pkg/logger"

// 中央「执行→转发」的 Risk V2 影子观测补采（仅观测，与计费解耦）。
//
// 转发到 cell 的请求由 EdgeForward 中间件反向代理并 c.Abort()，本地成功记账路径
// (recordUsageCore → enqueueRiskV2) 从不执行 → Risk V2 采集会漏掉这部分流量。
// 这里用 cell 回传的权威用量(EdgeUsageEnvelope) + 中央侧从请求体算出的请求侧特征
// (feat.V2Request) 拼出一个 synthetic UsageLog，复用现成 enqueueRiskV2 入队一条观测。
//
// 红线：只观测(EffectiveAction 恒 NONE)、不计费、不改余额、不影响已回传响应；
// 全程 flag 门控 + best-effort：dispatcher / 请求侧特征缺失即静默跳过。

// EnqueueForwardedRiskV2 为「转发成功」的请求补一条 Risk V2 观测。usage 来自 cell
// 权威 envelope，请求侧特征(feat.V2Request)由 handler 从缓冲请求体算出。
func (s *GatewayService) EnqueueForwardedRiskV2(apiKey *APIKey, env EdgeUsageEnvelope, feat RiskUsageFeatures) {
	// TODO(risk-v2 diag): 临时诊断日志，定位后移除。
	if s == nil || s.riskV2Dispatcher == nil || !s.riskV2Cfg.EffectiveEnabled() || apiKey == nil || feat.V2Request == nil {
		logger.LegacyPrintf("service.risk_v2",
			"[risk_v2.fwd_enqueue] SKIP dispatcher_nil=%v effective=%v apikey_nil=%v v2req_nil=%v",
			s == nil || s.riskV2Dispatcher == nil, s != nil && s.riskV2Cfg.EffectiveEnabled(), apiKey == nil, feat.V2Request == nil)
		return
	}
	logger.LegacyPrintf("service.risk_v2", "[risk_v2.fwd_enqueue] OK enqueue user=%d platform=%s", apiKey.UserID, env.Platform)
	// synthetic UsageLog：只填 enqueueRiskV2 读取的字段；用量口径以 cell 权威 envelope 为准。
	usageLog := &UsageLog{
		UserID:              apiKey.UserID,
		APIKeyID:            apiKey.ID,
		Model:               env.Model,
		Stream:              env.Stream,
		InputTokens:         env.Usage.InputTokens,
		OutputTokens:        env.Usage.OutputTokens,
		CacheCreationTokens: env.Usage.CacheCreationInputTokens,
		CacheReadTokens:     env.Usage.CacheReadInputTokens,
	}
	s.enqueueRiskV2(usageLog, feat, env.Platform)
}
