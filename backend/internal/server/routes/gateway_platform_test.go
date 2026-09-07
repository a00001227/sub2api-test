package routes

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// TestGetGroupPlatform 锁死路由分流的平台判定语义。
//
// 核心回归护栏：ForcePlatform 缺席时，getGroupPlatform 的行为必须与引入
// edge-forward 平台透传之前逐字节相同（claude / 普通请求不受影响）；
// 只有携带 ForcePlatform 的请求（/antigravity 或 EDGE cell 中央转发的 openai）
// 才走强制平台分支。
func TestGetGroupPlatform(t *testing.T) {
	gin.SetMode(gin.TestMode)

	setAPIKey := func(c *gin.Context, platform string, hasGroup bool) {
		key := &service.APIKey{}
		if hasGroup {
			key.Group = &service.Group{Platform: platform}
		}
		c.Set(string(middleware.ContextKeyAPIKey), key)
	}

	cases := []struct {
		name          string
		hasAPIKey     bool
		hasGroup      bool
		groupPlatform string
		forcePlatform string // "" = 未设置
		want          string
	}{
		// —— ForcePlatform 缺席：与旧逻辑必须完全一致 ——
		{name: "no_apikey", hasAPIKey: false, want: ""},
		{name: "claude_edge_group_nil", hasAPIKey: true, hasGroup: false, want: ""},
		{name: "central_anthropic_group", hasAPIKey: true, hasGroup: true, groupPlatform: service.PlatformAnthropic, want: service.PlatformAnthropic},
		{name: "central_openai_group", hasAPIKey: true, hasGroup: true, groupPlatform: service.PlatformOpenAI, want: service.PlatformOpenAI},

		// —— ForcePlatform 存在：强制平台优先（新行为，只此一类请求受影响）——
		{name: "edge_openai_forced_group_nil", hasAPIKey: true, hasGroup: false, forcePlatform: service.PlatformOpenAI, want: service.PlatformOpenAI},
		{name: "force_wins_over_group", hasAPIKey: true, hasGroup: true, groupPlatform: service.PlatformAnthropic, forcePlatform: service.PlatformOpenAI, want: service.PlatformOpenAI},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			if tc.hasAPIKey {
				setAPIKey(c, tc.groupPlatform, tc.hasGroup)
			}
			if tc.forcePlatform != "" {
				c.Set(string(middleware.ContextKeyForcePlatform), tc.forcePlatform)
			}
			if got := getGroupPlatform(c); got != tc.want {
				t.Fatalf("getGroupPlatform() = %q, want %q", got, tc.want)
			}
		})
	}
}
