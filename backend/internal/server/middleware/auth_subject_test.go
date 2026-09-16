package middleware

import (
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func intPtr(v int) *int { return &v }

func newSubjectTestContext(apiKey *service.APIKey, subject AuthSubject, forcePlatform string) *gin.Context {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/messages", nil)
	if apiKey != nil {
		c.Set(string(ContextKeyAPIKey), apiKey)
	}
	c.Set(string(ContextKeyUser), subject)
	if forcePlatform != "" {
		c.Set(string(ContextKeyForcePlatform), forcePlatform)
	}
	return c
}

// 平台专属并发只收录设了值的平台；未设的平台沿用全局值。
func TestAuthSubjectFromUser_PlatformConcurrency(t *testing.T) {
	u := &service.User{ID: 7, Concurrency: 5, PlatformLimits: map[string]service.UserPlatformLimit{
		service.PlatformAnthropic: {Concurrency: intPtr(2)},
		service.PlatformOpenAI:    {RPMLimit: intPtr(30)}, // 只设了 RPM，不进并发 map
	}}
	s := authSubjectFromUser(u)
	require.Equal(t, int64(7), s.UserID)
	require.Equal(t, 5, s.Concurrency)
	require.Equal(t, map[string]int{service.PlatformAnthropic: 2}, s.PlatformConcurrency)

	require.Nil(t, authSubjectFromUser(&service.User{ID: 1, Concurrency: 3}).PlatformConcurrency)
}

// 分组平台命中专属值 → 平台槽位；未命中 → 共享槽位（platform=""，全局上限）。
func TestResolveUserSlot(t *testing.T) {
	subject := AuthSubject{UserID: 7, Concurrency: 5, PlatformConcurrency: map[string]int{service.PlatformOpenAI: 2}}

	// openai 分组：专属槽位
	c := newSubjectTestContext(&service.APIKey{Group: &service.Group{Platform: service.PlatformOpenAI}}, subject, "")
	uid, platform, max := ResolveUserSlot(c)
	require.Equal(t, int64(7), uid)
	require.Equal(t, service.PlatformOpenAI, platform)
	require.Equal(t, 2, max)

	// anthropic 分组：没设专属值 → 共享槽位
	c = newSubjectTestContext(&service.APIKey{Group: &service.Group{Platform: service.PlatformAnthropic}}, subject, "")
	uid, platform, max = ResolveUserSlot(c)
	require.Equal(t, int64(7), uid)
	require.Equal(t, "", platform)
	require.Equal(t, 5, max)

	// ForcePlatform 优先于分组平台
	c = newSubjectTestContext(&service.APIKey{Group: &service.Group{Platform: service.PlatformAnthropic}}, subject, service.PlatformOpenAI)
	_, platform, max = ResolveUserSlot(c)
	require.Equal(t, service.PlatformOpenAI, platform)
	require.Equal(t, 2, max)

	// 无分组、无 ForcePlatform → 共享槽位
	c = newSubjectTestContext(&service.APIKey{}, subject, "")
	_, platform, max = ResolveUserSlot(c)
	require.Equal(t, "", platform)
	require.Equal(t, 5, max)

	// 无身份
	gin.SetMode(gin.TestMode)
	empty, _ := gin.CreateTestContext(httptest.NewRecorder())
	uid, platform, max = ResolveUserSlot(empty)
	require.Equal(t, int64(0), uid)
	require.Equal(t, "", platform)
	require.Equal(t, 0, max)
}
