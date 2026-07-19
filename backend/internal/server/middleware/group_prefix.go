package middleware

import (
	"context"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// gatewayRemainderAllowed 前缀剥离后的剩余路径必须指向网关协议端点，
// 管理类 API（/balance、/sub-keys…）不参与前缀路由，避免语义歧义。
func gatewayRemainderAllowed(rest string) bool {
	return strings.HasPrefix(rest, "/v1/") ||
		strings.HasPrefix(rest, "/v1beta/") ||
		rest == "/responses" || strings.HasPrefix(rest, "/responses/") ||
		strings.HasPrefix(rest, "/chat/") ||
		rest == "/embeddings" ||
		strings.HasPrefix(rest, "/images/")
}

// GroupPrefixRewrite 分组 URL 前缀重写中间件。
//
// 处理形如 /{slug}/v1/messages 的请求：将 slug 解析为分组，写入
// ctxkey.ForcedGroup，剥掉前缀后通过 engine.HandleContext 重新分发。
// API Key 认证中间件读取 ForcedGroup 后把本次请求的计费/调度分组替换掉。
//
// 必须注册为第一个全局中间件：gin 的路由匹配发生在中间件链之前，
// 带前缀的路径不会命中任何注册路由，只有在链条最前端拦截、改写 path 并
// 重新分发，后续的日志/CORS/前端中间件才会基于剥离后的路径各跑一次。
// 重入由 ForcedGroup 上下文值防护（第二次进入直接放行）。
func GroupPrefixRewrite(engine *gin.Engine, resolver *service.GroupSlugResolver) gin.HandlerFunc {
	return func(c *gin.Context) {
		if engine == nil || resolver == nil {
			c.Next()
			return
		}
		// 重入守卫：已重写过的请求直接放行
		if fg, ok := c.Request.Context().Value(ctxkey.ForcedGroup).(*service.Group); ok && fg != nil {
			c.Next()
			return
		}

		path := c.Request.URL.Path
		if len(path) < 2 || path[0] != '/' {
			c.Next()
			return
		}
		idx := strings.IndexByte(path[1:], '/')
		if idx <= 0 {
			c.Next()
			return
		}
		seg := path[1 : 1+idx]
		rest := path[1+idx:]

		if service.IsReservedGroupSlug(seg) {
			c.Next()
			return
		}
		if !gatewayRemainderAllowed(rest) {
			c.Next()
			return
		}

		group, err := resolver.Resolve(c.Request.Context(), seg)
		if err != nil {
			// 解析失败（缓存为空且回源出错）：按普通路径继续，避免 DB 抖动
			// 期间误伤合法 slug 请求
			c.Next()
			return
		}
		if group == nil {
			// 网关形路径 + 未注册的 slug（多为 Base URL 拼错）：直接返回 JSON
			// 404。若放行会落到 SPA 兜底返回 index.html + 200，SDK 端表现为
			// JSON 解析错误，极难排查。
			AbortWithError(c, 404, "UNKNOWN_CHANNEL",
				"Unknown channel prefix '/"+seg+"'. Check the base URL against your channel list.")
			return
		}

		ctx := context.WithValue(c.Request.Context(), ctxkey.ForcedGroup, group)
		c.Request = c.Request.WithContext(ctx)
		c.Request.URL.Path = rest
		if c.Request.URL.RawPath != "" {
			// 编码路径同步剥前缀；无法对齐时清空让 net/http 回退使用 Path
			if strings.HasPrefix(c.Request.URL.RawPath, "/"+seg) {
				c.Request.URL.RawPath = strings.TrimPrefix(c.Request.URL.RawPath, "/"+seg)
			} else {
				c.Request.URL.RawPath = ""
			}
		}

		engine.HandleContext(c)
		c.Abort()
	}
}

// GetForcedGroupFromContext 读取 URI 前缀选定的分组（无则返回 nil,false）。
func GetForcedGroupFromContext(c *gin.Context) (*service.Group, bool) {
	fg, ok := c.Request.Context().Value(ctxkey.ForcedGroup).(*service.Group)
	if !ok || fg == nil {
		return nil, false
	}
	return fg, true
}
