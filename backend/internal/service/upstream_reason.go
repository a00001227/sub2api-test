package service

import (
	"strconv"
	"strings"
)

// upstreamReasonMaxRunes 下发给客户端的上游原因最大长度(rune)。
const upstreamReasonMaxRunes = 300

// WithUpstreamReason 把上游真实原因(脱敏、截断)拼到兜底文案后面下发给客户端:
//
//	"<fallback> (upstream <status>) — <reason>"
//
// 以前 Claude 路径对 429/529/5xx/未知状态码只回一句固定文案("Upstream request failed"),用户和
// 运维都看不出到底是什么(得翻服务器日志);OpenAI 路径早已拼真实原因(handler.withUpstreamReason)。
// upstreamStatus>0 才写状态码(传输层失败无状态码);reason 为空时原样返回 fallback。
func WithUpstreamReason(fallback string, upstreamStatus int, upstreamMsg string) string {
	reason := strings.TrimSpace(sanitizeUpstreamErrorMessage(upstreamMsg))
	if rs := []rune(reason); len(rs) > upstreamReasonMaxRunes {
		reason = strings.TrimSpace(string(rs[:upstreamReasonMaxRunes])) + "…"
	}
	out := fallback
	if upstreamStatus > 0 {
		out += " (upstream " + strconv.Itoa(upstreamStatus) + ")"
	}
	if reason == "" {
		return out
	}
	return out + " — " + reason
}
