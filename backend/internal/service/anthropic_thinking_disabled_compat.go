package service

import (
	"strings"
	"sync"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// 「thinking.type.disabled 不支持」兼容层。
//
// 新一代 Claude 模型不再接受关闭 thinking,只认 adaptive。上游原文:
//
//	"thinking.type.disabled" is not supported for this model. Use "thinking.type.adaptive" and "output_config.effort"
//
// 老版本 Claude Code / 第三方 SDK 在用户关掉 thinking 时仍发 {type:"disabled"},每个请求都被
// 上游 400 打回,客户端只看到一条看不懂的报错。处理分两步:
//
//  1. 反应式:上游回该 400 时,把 thinking 改写成 adaptive(去掉 budget_tokens、补
//     output_config.effort=low 贴近"不要思考"的原意),同号重试一次。
//  2. 记忆:把该(映射后)模型记进进程内表;后续同模型请求发出前就改写,不再白跑一次上游。
//     不维护静态模型清单——上游拒一次就学会,新模型无需改代码;进程重启后重新学习(代价
//     仅一次 400)。
const thinkingDisabledCompatEffort = "low"

var thinkingDisabledUnsupportedModels sync.Map // map[modelLower]struct{}

// isThinkingDisabledUnsupportedError 识别上游「thinking.type.disabled 不支持」的 400 文案。
func isThinkingDisabledUnsupportedError(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "thinking.type.disabled") && strings.Contains(m, "not supported")
}

// markThinkingDisabledUnsupported 记住该模型拒收 thinking.type=disabled。
func markThinkingDisabledUnsupported(model string) {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return
	}
	thinkingDisabledUnsupportedModels.Store(model, struct{}{})
}

// modelRejectsThinkingDisabled 该模型是否已知拒收 thinking.type=disabled。
func modelRejectsThinkingDisabled(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return false
	}
	_, ok := thinkingDisabledUnsupportedModels.Load(model)
	return ok
}

// RewriteThinkingDisabledToAdaptive 把 thinking:{type:"disabled"} 改写成上游要求的形态:
// thinking 整体替换为 {type:"adaptive"}(顺带丢掉 budget_tokens 等 disabled/enabled 才有的字段),
// output_config.effort 缺省时补 low(用户本意是不思考,取最低档最接近)。
// 返回 (改写后 body, true);thinking 不是 disabled 时原样返回 (body, false)。
func RewriteThinkingDisabledToAdaptive(body []byte) ([]byte, bool) {
	if !strings.EqualFold(strings.TrimSpace(gjson.GetBytes(body, "thinking.type").String()), "disabled") {
		return body, false
	}
	modified, err := sjson.SetRawBytes(body, "thinking", []byte(`{"type":"adaptive"}`))
	if err != nil {
		return body, false
	}
	if strings.TrimSpace(gjson.GetBytes(modified, "output_config.effort").String()) == "" {
		if withEffort, err := sjson.SetBytes(modified, "output_config.effort", thinkingDisabledCompatEffort); err == nil {
			modified = withEffort
		}
	}
	return modified, true
}
