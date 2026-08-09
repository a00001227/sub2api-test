package service

import (
	"encoding/json"
	"fmt"
)

/*
Edge usage carrier (#86b):中央「执行→转发」时,消费者计费需要用量,但账号在 cell、
执行也在 cell。cell 是权威计费源(和发给 Portal 的 provider 收益同源),所以由 cell 把
算好的 usage 带回中央,中央据此给消费者计费(不再自己解析 SSE,避免和 cell 口径漂移)。

载体(cell 全球公网回连中央,不能用 HTTP trailer——中间 nginx/LB 会剥):
  - 流式:响应末尾追加一个 SSE 事件 `event: sub2api_usage`(在 message_stop 之后)。
    中央 EdgeForward 读到后消费并**剥掉**,不透传给消费者客户端。
  - 非流式:响应头 `X-Sub2api-Usage`(usage 在写 body 前已知,可用头)。

仅在 edge-trusted(来自中央可信转发)时才吐;直连消费者不吐。
*/

// EdgeUsageEventName 是 cell 末尾用量事件的 SSE event 名(中央据此识别并剥掉)。
const EdgeUsageEventName = "sub2api_usage"

// EdgeUsageHeader 是非流式响应携带用量的头名。
const EdgeUsageHeader = "X-Sub2api-Usage"

// EdgeUsageEnvelope 是 cell → 中央 的权威用量载荷。字段足以让中央重建一个
// ForwardResult 交给现成 RecordUsage 给消费者计费(占位 account 方案:provider
// outbox 因占位号无 external ref 天然跳过)。
type EdgeUsageEnvelope struct {
	// Platform 决定中央用哪条成本路径计费:"openai" → OpenAI 成本路径(处理
	// image_input/service_tier);其余(空/anthropic)→ claude 成本路径。
	Platform      string      `json:"platform,omitempty"`
	Model         string      `json:"model"`
	UpstreamModel string      `json:"upstream_model,omitempty"`
	Usage         ClaudeUsage `json:"usage"`
	// OpenAI 专有(claude 用不到):图片输入 token 与 service_tier(定价档)。
	ImageInputTokens int    `json:"image_input_tokens,omitempty"`
	ServiceTier      string `json:"service_tier,omitempty"`
	ImageCount       int    `json:"image_count,omitempty"`
	Stream           bool   `json:"stream"`
}

// BuildEdgeUsageEnvelope 从 claude ForwardResult 提取计费所需字段。
func BuildEdgeUsageEnvelope(result *ForwardResult) EdgeUsageEnvelope {
	if result == nil {
		return EdgeUsageEnvelope{}
	}
	return EdgeUsageEnvelope{
		Platform:      PlatformAnthropic,
		Model:         result.Model,
		UpstreamModel: result.UpstreamModel,
		Usage:         result.Usage,
		ImageCount:    result.ImageCount,
		Stream:        result.Stream,
	}
}

// BuildEdgeUsageEnvelopeOpenAI 从 OpenAIForwardResult 提取计费所需字段。OpenAIUsage
// 的 token 桶映射到 ClaudeUsage 的同名字段,image_input/service_tier 单独带。
func BuildEdgeUsageEnvelopeOpenAI(result *OpenAIForwardResult) EdgeUsageEnvelope {
	if result == nil {
		return EdgeUsageEnvelope{}
	}
	tier := ""
	if result.ServiceTier != nil {
		tier = *result.ServiceTier
	}
	return EdgeUsageEnvelope{
		Platform:      PlatformOpenAI,
		Model:         result.Model,
		UpstreamModel: result.UpstreamModel,
		Usage: ClaudeUsage{
			InputTokens:              result.Usage.InputTokens,
			OutputTokens:             result.Usage.OutputTokens,
			CacheCreationInputTokens: result.Usage.CacheCreationInputTokens,
			CacheReadInputTokens:     result.Usage.CacheReadInputTokens,
			ImageOutputTokens:        result.Usage.ImageOutputTokens,
		},
		ImageInputTokens: result.Usage.ImageInputTokens,
		ServiceTier:      tier,
		ImageCount:       result.ImageCount,
		Stream:           result.Stream,
	}
}

// ToForwardResult 把 envelope 还原成中央 claude 计费用的 ForwardResult(RequestID
// 由中央自己填,用于消费者计费去重,不复用 cell 的)。
func (e EdgeUsageEnvelope) ToForwardResult() *ForwardResult {
	return &ForwardResult{
		Usage:         e.Usage,
		Model:         e.Model,
		UpstreamModel: e.UpstreamModel,
		Stream:        e.Stream,
		ImageCount:    e.ImageCount,
	}
}

// ToOpenAIForwardResult 把 envelope 还原成中央 OpenAI 计费用的 OpenAIForwardResult。
func (e EdgeUsageEnvelope) ToOpenAIForwardResult() *OpenAIForwardResult {
	var tier *string
	if e.ServiceTier != "" {
		t := e.ServiceTier
		tier = &t
	}
	return &OpenAIForwardResult{
		Model:         e.Model,
		UpstreamModel: e.UpstreamModel,
		Usage: OpenAIUsage{
			InputTokens:              e.Usage.InputTokens,
			ImageInputTokens:         e.ImageInputTokens,
			OutputTokens:             e.Usage.OutputTokens,
			CacheCreationInputTokens: e.Usage.CacheCreationInputTokens,
			CacheReadInputTokens:     e.Usage.CacheReadInputTokens,
			ImageOutputTokens:        e.Usage.ImageOutputTokens,
		},
		ServiceTier: tier,
		ImageCount:  e.ImageCount,
		Stream:      e.Stream,
	}
}

// IsOpenAI 报告该 envelope 是否走 OpenAI 成本路径。
func (e EdgeUsageEnvelope) IsOpenAI() bool { return e.Platform == PlatformOpenAI }

// MarshalJSON 无副作用的紧凑 JSON(用于 SSE data 行 / 响应头)。
func (e EdgeUsageEnvelope) marshal() ([]byte, error) {
	return json.Marshal(e)
}

// SSEBytes 返回可直接写进流末尾的 SSE 事件字节:`event: sub2api_usage\ndata: {...}\n\n`。
func (e EdgeUsageEnvelope) SSEBytes() ([]byte, error) {
	data, err := e.marshal()
	if err != nil {
		return nil, err
	}
	return []byte(fmt.Sprintf("event: %s\ndata: %s\n\n", EdgeUsageEventName, data)), nil
}

// HeaderValue 返回非流式响应头 X-Sub2api-Usage 的值(紧凑 JSON)。
func (e EdgeUsageEnvelope) HeaderValue() (string, error) {
	data, err := e.marshal()
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// ParseEdgeUsageEnvelope 中央侧:从 SSE data 行 / 头值还原 envelope。
func ParseEdgeUsageEnvelope(data []byte) (EdgeUsageEnvelope, error) {
	var e EdgeUsageEnvelope
	if err := json.Unmarshal(data, &e); err != nil {
		return EdgeUsageEnvelope{}, err
	}
	return e, nil
}
