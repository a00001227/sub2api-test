package middleware

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 先给 10 字节再报 unexpected EOF:模拟客户端上传到一半断开。
type truncatedBody struct {
	r    io.Reader
	done bool
}

func (t *truncatedBody) Read(p []byte) (int, error) {
	if t.done {
		return 0, io.ErrUnexpectedEOF
	}
	n, err := t.r.Read(p)
	if err == io.EOF {
		t.done = true
		return n, nil
	}
	return n, err
}
func (t *truncatedBody) Close() error { return nil }

func newBodyReadCtx(t *testing.T, path string, body io.ReadCloser, contentLength int64) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, path, body)
	c.Request.ContentLength = contentLength
	return c, w
}

// 上传断开 → 400 说明收了多少字节、标客户端侧;不再把半截 body 交给下游。
func TestReadRequestBodyOrAbort_TruncatedUploadIs400(t *testing.T) {
	c, w := newBodyReadCtx(t, "/v1/messages", &truncatedBody{r: strings.NewReader("0123456789")}, 500)
	body, ok := readRequestBodyOrAbort(c)
	require.False(t, ok)
	require.Nil(t, body)
	require.True(t, c.IsAborted())
	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Contains(t, w.Body.String(), "request body incomplete: received 10 of 500 bytes")
	require.Contains(t, w.Body.String(), `"type":"error"`, "anthropic 协议格式")
	require.True(t, service.HasOpsClientBusinessLimited(c))
}

// 超过 bodyLimit → 413。
func TestReadRequestBodyOrAbort_TooLargeIs413(t *testing.T) {
	c, w := newBodyReadCtx(t, "/v1/responses", nil, 20)
	c.Request.Body = http.MaxBytesReader(w, io.NopCloser(strings.NewReader("01234567890123456789")), 5)
	_, ok := readRequestBodyOrAbort(c)
	require.False(t, ok)
	require.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
	require.NotContains(t, w.Body.String(), `"type":"error"`, "openai 协议格式无顶层 type")
	require.Contains(t, w.Body.String(), "Request exceeds the maximum size")
}

// 正常读完 → 还原可再读。
func TestReadRequestBodyOrAbort_RestoresBody(t *testing.T) {
	c, _ := newBodyReadCtx(t, "/v1/messages", io.NopCloser(strings.NewReader(`{"model":"m"}`)), 13)
	body, ok := readRequestBodyOrAbort(c)
	require.True(t, ok)
	require.Equal(t, `{"model":"m"}`, string(body))
	again, err := io.ReadAll(c.Request.Body)
	require.NoError(t, err)
	require.Equal(t, `{"model":"m"}`, string(again))
}
