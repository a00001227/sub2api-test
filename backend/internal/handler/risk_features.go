package handler

import "github.com/Wei-Shaw/sub2api/internal/service"

// riskSimhashCaptureBytes 限制从消息 raw 拷贝的字节数（首 8KB）。simhash 归一化
// 内部还会再截断，这里先限拷贝量以免大请求体在异步闭包里长时间保活。
const riskSimhashCaptureBytes = 8 * 1024

// buildRiskUsageFeatures 从已解析请求提取 Risk Phase 0（仅观测）请求特征。
// 必须在 parsedReq 仍有效、请求体未被复用时调用，并把结果作为标量/独立副本捕获进
// 响应后闭包——绝不在异步闭包里持有 parsedReq/请求体引用。
// 隐私：只取计数 + 消息 raw 的截断副本（供异步计算 64 位 simhash），绝不落原文。
func buildRiskUsageFeatures(parsed *service.ParsedRequest) service.RiskUsageFeatures {
	if parsed == nil {
		return service.RiskUsageFeatures{}
	}
	feat := service.RiskUsageFeatures{
		MessageCount: parsed.MessageCount(),
		MaxTokens:    parsed.MaxTokens,
		Temperature:  parsed.Temperature,
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
	return feat
}
