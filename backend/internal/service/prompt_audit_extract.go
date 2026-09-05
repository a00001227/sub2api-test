package service

import (
	"strings"

	"github.com/tidwall/gjson"
)

// ExtractPromptAuditContent 从请求体抽取提示词审计留存文本 + 消息条数。
//
// 复用内容审核的 per-protocol collector（同包私有函数），但不套用审核用的
// maxModerationInputRunes 截断——提示词审计要尽量留全文，上限交由 service 层
// 的 maxPromptAuditFullPromptRunes 统一收口，避免单条超大请求撑爆一行。
//
// 说明：anthropic/openai_chat 抽取全部 user 消息；responses/gemini 抽取当轮
// 最新输入（与内容审核一致），已足够覆盖运营“看用户发了什么”的诉求。
func ExtractPromptAuditContent(protocol string, body []byte) (text string, messageCount int) {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return "", 0
	}
	var parts []string
	var images []string
	switch protocol {
	case ContentModerationProtocolAnthropicMessages:
		collectAllAnthropicUserMessages(gjson.GetBytes(body, "messages"), &parts, &images)
		messageCount = int(gjson.GetBytes(body, "messages.#").Int())
	case ContentModerationProtocolOpenAIChat:
		collectAllRoleMessages(gjson.GetBytes(body, "messages"), "user", &parts, &images)
		messageCount = int(gjson.GetBytes(body, "messages.#").Int())
	case ContentModerationProtocolOpenAIResponses:
		collectLastResponsesInput(gjson.GetBytes(body, "input"), &parts, &images)
		messageCount = int(gjson.GetBytes(body, "input.#").Int())
	case ContentModerationProtocolGemini:
		collectLastGeminiContent(gjson.GetBytes(body, "contents"), &parts, &images)
		messageCount = int(gjson.GetBytes(body, "contents.#").Int())
	case ContentModerationProtocolOpenAIImages:
		addModerationText(&parts, gjson.GetBytes(body, "prompt").String())
	default:
		collectLastResponsesInput(gjson.GetBytes(body, "input"), &parts, &images)
		collectLastRoleMessage(gjson.GetBytes(body, "messages"), "user", &parts, &images)
		collectLastGeminiContent(gjson.GetBytes(body, "contents"), &parts, &images)
	}
	text = normalizeContentModerationText(strings.Join(parts, "\n"))
	return text, messageCount
}
