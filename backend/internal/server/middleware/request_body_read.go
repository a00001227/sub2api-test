package middleware

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// readRequestBodyOrAbort 读完整请求体并原样还原(供后续中间件 / handler 复用)。
//
// 读失败不再吞掉,按成因直接回明确错误并 Abort:
//   - 超过 bodyLimit(http.MaxBytesError)→ 413 "Request exceeds the maximum size";
//   - 其它(unexpected EOF / connection reset / 客户端断开)→ 400 "request body incomplete:
//     received N of M bytes"。
//
// 以前审核/审计中间件把读错误吞掉、把半截 body 还原给下游继续走:半截 JSON 缺 model 字段 →
// 转发白名单 403 "request has no model field";或走到并发槽位时 ctx 已取消 → 499 "context
// canceled"。两种都把「客户端上传中途断了」伪装成别的错,运维面板上查不出真因。
// 两类都是客户端侧成因,标记业务限制、排除出 SLA。
func readRequestBodyOrAbort(c *gin.Context) ([]byte, bool) {
	if c.Request == nil || c.Request.Body == nil {
		return nil, true
	}
	buf, err := io.ReadAll(c.Request.Body)
	_ = c.Request.Body.Close()
	if err != nil {
		abortRequestBodyReadError(c, err, len(buf))
		return nil, false
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(buf))
	return buf, true
}

// abortRequestBodyReadError 请求体读失败的统一收口(见 readRequestBodyOrAbort)。got = 已收到字节数。
func abortRequestBodyReadError(c *gin.Context, err error, got int) {
	openAIStyle := false
	switch moderationProtocolForPath(c.FullPath(), c.Request.URL.Path) {
	case service.ContentModerationProtocolOpenAIChat, service.ContentModerationProtocolOpenAIResponses, service.ContentModerationProtocolOpenAIImages:
		openAIStyle = true
	}
	var maxBytesErr *http.MaxBytesError
	if errors.As(err, &maxBytesErr) {
		service.MarkOpsClientBusinessLimited(c, service.OpsClientBusinessLimitedReasonRequestTooLarge)
		writeClientRequestError(c, http.StatusRequestEntityTooLarge, "invalid_request_error", "Request exceeds the maximum size", openAIStyle)
		return
	}
	expected := c.Request.ContentLength
	msg := fmt.Sprintf("request body incomplete: received %d of %d bytes (client upload aborted: %v)", got, expected, err)
	service.MarkOpsClientBusinessLimited(c, service.OpsClientBusinessLimitedReasonClientUploadAborted)
	writeClientRequestError(c, http.StatusBadRequest, "invalid_request_error", msg, openAIStyle)
}

func writeClientRequestError(c *gin.Context, status int, errType, message string, openAIStyle bool) {
	c.Abort()
	if openAIStyle {
		c.JSON(status, gin.H{"error": gin.H{"type": errType, "message": message}})
		return
	}
	c.JSON(status, gin.H{"type": "error", "error": gin.H{"type": errType, "message": message}})
}
