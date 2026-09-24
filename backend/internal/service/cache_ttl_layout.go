package service

import (
	"fmt"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// anthropicWireBodyKey 存本次发往 Anthropic 的最终请求体(经全部改写后),供 400 诊断用。
const anthropicWireBodyKey = "anthropic_wire_body"

// isCacheTTLOrderingError 识别 Anthropic 的缓存 ttl 顺序错误:
// "a ttl='1h' cache_control block must not come after a ttl='5m' cache_control block"。
func isCacheTTLOrderingError(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "cache_control") && strings.Contains(m, "must not come after")
}

// cacheControlTTLLayout 把请求体里所有带 cache_control 的块按 Anthropic 处理顺序
// (tools → system → messages)列成一行,形如 "tools[3]=5m system[0]=- messages[12].0=1h"
// ("-" = 有 cache_control 但没写 ttl,上游按 5m 处理)。用于定位「1h 排在 5m 之后」到底是
// 客户端自己写的还是我们注入的。
func cacheControlTTLLayout(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var parts []string
	add := func(label string, block gjson.Result) {
		cc := block.Get("cache_control")
		if !cc.Exists() {
			return
		}
		ttl := cc.Get("ttl").String()
		if ttl == "" {
			ttl = "-"
		}
		parts = append(parts, label+"="+ttl)
	}
	if tools := gjson.GetBytes(body, "tools"); tools.IsArray() {
		i := -1
		tools.ForEach(func(_, t gjson.Result) bool { i++; add(fmt.Sprintf("tools[%d]", i), t); return true })
	}
	if sys := gjson.GetBytes(body, "system"); sys.IsArray() {
		i := -1
		sys.ForEach(func(_, b gjson.Result) bool { i++; add(fmt.Sprintf("system[%d]", i), b); return true })
	}
	if msgs := gjson.GetBytes(body, "messages"); msgs.IsArray() {
		mi := -1
		msgs.ForEach(func(_, m gjson.Result) bool {
			mi++
			if content := m.Get("content"); content.IsArray() {
				ci := -1
				content.ForEach(func(_, b gjson.Result) bool { ci++; add(fmt.Sprintf("messages[%d].%d", mi, ci), b); return true })
			}
			return true
		})
	}
	return strings.Join(parts, " ")
}

func setAnthropicWireBody(c *gin.Context, body []byte) {
	if c != nil && len(body) > 0 {
		c.Set(anthropicWireBodyKey, body)
	}
}

func anthropicWireBodyFrom(c *gin.Context) []byte {
	if c == nil {
		return nil
	}
	if v, ok := c.Get(anthropicWireBodyKey); ok {
		if b, ok := v.([]byte); ok {
			return b
		}
	}
	return nil
}
