package service

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func newProtocolErrorTestCtx(t *testing.T) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	return c, rec
}

// 上游 200 流终态 response.failed 带 "overloaded" 文案:回 503 overloaded_error,
// 并落 ops slug + edge 头,让 cell / 中央都把它归为上游过载而非中转错误。
func TestWriteOpenAINonStreamingProtocolError_OverloadedMapsTo503(t *testing.T) {
	s := &OpenAIGatewayService{}
	c, rec := newProtocolErrorTestCtx(t)
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}}

	err := s.writeOpenAINonStreamingProtocolError(resp, c, nil, "Our servers are currently overloaded. Please try again later.")
	require.Error(t, err)

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	body := rec.Body.String()
	require.Equal(t, "overloaded_error", gjson.Get(body, "error.type").String())
	require.Contains(t, gjson.Get(body, "error.message").String(), "overloaded")

	slug, ok := c.Get(OpsUpstreamCauseSlugKey)
	require.True(t, ok)
	require.Equal(t, UpstreamCauseOverloaded, slug)
	require.Equal(t, "200|"+UpstreamCauseOverloaded, rec.Header().Get(EdgeUpstreamCauseHeader))
}

// 其它文案(如我们自己转换出错被上游拒)维持原样:502 upstream_error,不打标、不排除出 SLA。
func TestWriteOpenAINonStreamingProtocolError_OtherStays502Unclassified(t *testing.T) {
	s := &OpenAIGatewayService{}
	c, rec := newProtocolErrorTestCtx(t)
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}}

	err := s.writeOpenAINonStreamingProtocolError(resp, c, nil, "No tool call found for function call output with call_id abc")
	require.Error(t, err)

	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.Equal(t, "upstream_error", gjson.Get(rec.Body.String(), "error.type").String())
	_, ok := c.Get(OpsUpstreamCauseSlugKey)
	require.False(t, ok)
	require.Empty(t, rec.Header().Get(EdgeUpstreamCauseHeader))
}

// OpenAI 网关级内部错误模板(200 流终态里出现,无 5xx 可依据):按文案归 other_5xx,
// 客户端仍收 502 upstream_error(可重试),但打标后不计入中转错误率。
func TestWriteOpenAINonStreamingProtocolError_OpenAIInternalErrorClassifiedOther5xx(t *testing.T) {
	s := &OpenAIGatewayService{}
	c, rec := newProtocolErrorTestCtx(t)
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}}

	msg := "An error occurred while processing your request. You can retry your request, or contact us through our help center at help.openai.com if the error persists. Please include the request ID ee4640ef-a3a0-4cac-b873-17ba4149ef3b in your message."
	err := s.writeOpenAINonStreamingProtocolError(resp, c, nil, msg)
	require.Error(t, err)

	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.Equal(t, "upstream_error", gjson.Get(rec.Body.String(), "error.type").String())
	slug, ok := c.Get(OpsUpstreamCauseSlugKey)
	require.True(t, ok)
	require.Equal(t, UpstreamCauseOther5xx, slug)
	require.Equal(t, "200|"+UpstreamCauseOther5xx, rec.Header().Get(EdgeUpstreamCauseHeader))
}

func TestClassifyUpstreamCause_OpenAIInternalErrorTemplate(t *testing.T) {
	require.Equal(t, UpstreamCauseOther5xx, ClassifyUpstreamCause(200, "An error occurred while processing your request. You can retry your request, or contact us through our help center at help.openai.com if the error persists."))
	// 只有一半特征不算(避免误伤普通 4xx 文案)
	require.Equal(t, "", ClassifyUpstreamCause(200, "An error occurred while processing your request."))
	require.Equal(t, "", ClassifyUpstreamCause(400, "Invalid request: see help.openai.com"))
}
