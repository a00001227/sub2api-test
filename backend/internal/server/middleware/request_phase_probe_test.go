package middleware

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// 把 observer logger 注入请求 ctx(生产上由 RequestLogger 注入 request-scoped logger)。
func probeTestRouter(alertAfter time.Duration, handlers ...gin.HandlerFunc) (*gin.Engine, *observer.ObservedLogs) {
	gin.SetMode(gin.TestMode)
	core, logs := observer.New(zapcore.DebugLevel)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(logger.IntoContext(c.Request.Context(), zap.New(core)))
		c.Next()
	})
	r.Use(RequestBodyLimit(1 << 20))
	r.Use(RequestPhaseProbe(alertAfter))
	r.POST("/v1/messages", handlers...)
	return r, logs
}

func probeLogs(logs *observer.ObservedLogs, msg string) []observer.LoggedEntry {
	var out []observer.LoggedEntry
	for _, e := range logs.All() {
		if strings.Contains(e.Message, msg) {
			out = append(out, e)
		}
	}
	return out
}

func TestRequestPhaseProbe_FastRequestIsSilentButRecordsPhases(t *testing.T) {
	var timeline string
	var bytesRead int64
	r, logs := probeTestRouter(time.Second,
		Phase("api_key_auth", func(c *gin.Context) { c.Next() }),
		Phase("edge_forward", func(c *gin.Context) {
			SetRequestPhase(c, "edge_forward.read_body")
			b, err := io.ReadAll(c.Request.Body)
			require.NoError(t, err)
			require.Equal(t, "hello", string(b))
			p := requestPhaseProbeFrom(c)
			require.NotNil(t, p)
			p.mu.Lock()
			timeline = p.timelineStringLocked()
			p.mu.Unlock()
			bytesRead = p.body.bytesRead.Load()
			require.True(t, p.body.eof.Load())
			c.String(http.StatusOK, "ok")
		}),
	)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString("hello"))
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, int64(5), bytesRead)
	require.Regexp(t, `^entry@\d+ms > api_key_auth@\d+ms > edge_forward@\d+ms > edge_forward\.read_body@\d+ms$`, timeline)
	require.Empty(t, probeLogs(logs, "request_phase_probe"), "快请求不应打任何探针日志")
}

func TestRequestPhaseProbe_SlowRequestAlertsWithPhaseAndBodyProgress(t *testing.T) {
	r, logs := probeTestRouter(30*time.Millisecond,
		Phase("api_key_auth", func(c *gin.Context) { c.Next() }),
		Phase("edge_forward", func(c *gin.Context) {
			SetRequestPhase(c, "edge_forward.read_body")
			// 只读一半:模拟请求体没传完就卡住。
			buf := make([]byte, 3)
			_, _ = io.ReadFull(c.Request.Body, buf)
			SetRequestPhase(c, "edge_forward.user_slot")
			time.Sleep(90 * time.Millisecond)
			c.String(http.StatusOK, "ok")
		}),
	)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString("hello"))
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	alerts := probeLogs(logs, "请求超过阈值仍未完成")
	require.Len(t, alerts, 1, "超阈值应且仅应告警一次")
	af := alerts[0].ContextMap()
	require.Equal(t, "edge_forward.user_slot", af["phase"])
	require.Equal(t, int64(3), af["body_bytes_read"])
	require.Equal(t, int64(5), af["content_length"])
	require.Equal(t, false, af["body_eof"])
	require.Contains(t, af["timeline"], "edge_forward.read_body@")
	require.Equal(t, "/v1/messages", af["path"])

	dones := probeLogs(logs, "慢请求完成")
	require.Len(t, dones, 1)
	df := dones[0].ContextMap()
	require.Equal(t, int64(200), df["status"])
	require.GreaterOrEqual(t, df["elapsed_ms"].(int64), int64(90))
	require.Equal(t, "edge_forward.user_slot", df["phase"])
}

func TestRequestPhaseProbe_CompletionRecordsCtxErrAndBodyReadError(t *testing.T) {
	r, logs := probeTestRouter(20*time.Millisecond,
		Phase("edge_forward", func(c *gin.Context) {
			SetRequestPhase(c, "edge_forward.read_body")
			_, err := io.ReadAll(c.Request.Body)
			require.Error(t, err)
			time.Sleep(40 * time.Millisecond)
			c.Status(499)
		}),
	)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", io.NopCloser(&failingReader{err: io.ErrUnexpectedEOF})).WithContext(ctx)
	req.ContentLength = 100
	r.ServeHTTP(w, req)

	dones := probeLogs(logs, "慢请求完成")
	require.Len(t, dones, 1)
	df := dones[0].ContextMap()
	require.Equal(t, int64(499), df["status"])
	require.Equal(t, "context canceled", df["ctx_err"])
	require.Equal(t, io.ErrUnexpectedEOF.Error(), df["body_err"])
	require.Equal(t, int64(100), df["content_length"])
	require.Equal(t, int64(0), df["body_bytes_read"])
}

func TestRequestPhaseProbe_PhaseIsNoopWithoutProbe(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/x", Phase("auth", func(c *gin.Context) {
		SetRequestPhase(c, "inner")
		c.String(http.StatusOK, "ok")
	}))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/x", nil))
	require.Equal(t, http.StatusOK, w.Code)
}

type failingReader struct{ err error }

func (f *failingReader) Read([]byte) (int, error) { return 0, f.err }
