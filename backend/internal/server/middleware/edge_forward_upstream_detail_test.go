package middleware

import (
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func newDetailTestCtx(t *testing.T) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	return c
}

func opsMsg(c *gin.Context) string {
	v, _ := c.Get(service.OpsUpstreamErrorMessageKey)
	s, _ := v.(string)
	return s
}

func opsSlug(c *gin.Context) string {
	v, _ := c.Get(service.OpsUpstreamCauseSlugKey)
	s, _ := v.(string)
	return s
}

func opsEvents(c *gin.Context) []*service.OpsUpstreamErrorEvent {
	v, _ := c.Get(service.OpsUpstreamErrorsKey)
	evs, _ := v.([]*service.OpsUpstreamErrorEvent)
	return evs
}

// 中央剥取 cell 的摘要头:上游原话进 message,账号名/平台进上游事件;account_id 不进 ops 行。
func TestRecordEdgeUpstreamDetail_WritesMessageAndEvent(t *testing.T) {
	c := newDetailTestCtx(t)
	hv := service.EncodeEdgeUpstreamDetail(&service.EdgeUpstreamDetail{
		Status: 403, Message: "Overloaded 中文", Platform: "anthropic",
		AccountID: 7, AccountName: "pa_abc", RequestID: "req_1", Kind: "stream_error",
	})
	recordEdgeUpstreamDetail(c, hv)

	require.Equal(t, "Overloaded 中文", opsMsg(c))
	v, _ := c.Get(service.OpsUpstreamStatusCodeKey)
	require.Equal(t, 403, v)
	evs := opsEvents(c)
	require.Len(t, evs, 1)
	require.Equal(t, "pa_abc", evs[0].AccountName)
	require.Equal(t, int64(7), evs[0].AccountID)
	require.Equal(t, "anthropic", evs[0].Platform)
	require.Equal(t, "req_1", evs[0].UpstreamRequestID)
	require.Equal(t, "stream_error", evs[0].Kind)
}

// 两个头到达顺序无关:最终 message 都是上游原话,slug 都在权威 key 里。
func TestRecordEdgeUpstreamCauseAndDetail_OrderIndependent(t *testing.T) {
	hv := service.EncodeEdgeUpstreamDetail(&service.EdgeUpstreamDetail{Status: 529, Message: "Overloaded"})

	c1 := newDetailTestCtx(t)
	recordEdgeUpstreamCause(c1, "529|overloaded")
	recordEdgeUpstreamDetail(c1, hv)
	require.Equal(t, "Overloaded", opsMsg(c1))
	require.Equal(t, "overloaded", opsSlug(c1))

	c2 := newDetailTestCtx(t)
	recordEdgeUpstreamDetail(c2, hv)
	recordEdgeUpstreamCause(c2, "529|overloaded")
	require.Equal(t, "Overloaded", opsMsg(c2))
	require.Equal(t, "overloaded", opsSlug(c2))
}

// 只有 Cause 头(旧版 cell):行为与以前一致,message 用 slug 兜底。
func TestRecordEdgeUpstreamCause_LegacyCellOnlySlug(t *testing.T) {
	c := newDetailTestCtx(t)
	recordEdgeUpstreamCause(c, "503|proxy_down")
	require.Equal(t, "proxy_down", opsMsg(c))
	require.Equal(t, "proxy_down", opsSlug(c))
	require.Empty(t, opsEvents(c))
}

func TestRecordEdgeUpstreamDetail_IgnoresGarbage(t *testing.T) {
	c := newDetailTestCtx(t)
	recordEdgeUpstreamDetail(c, "%zz not-json")
	recordEdgeUpstreamDetail(c, "")
	require.Empty(t, opsMsg(c))
	require.Empty(t, opsEvents(c))
}
