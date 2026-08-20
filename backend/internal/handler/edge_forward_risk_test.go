package handler

import (
	"bytes"
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// captureSink 实现 service.RiskV2Sink，捕获入队的观测包。
type captureSink struct {
	mu   sync.Mutex
	envs []service.RiskFeatureEnvelope
}

func (s *captureSink) Consume(env service.RiskFeatureEnvelope) {
	s.mu.Lock()
	s.envs = append(s.envs, env)
	s.mu.Unlock()
}
func (s *captureSink) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.envs)
}

func newForwardRiskHandler(t *testing.T) (*GatewayHandler, *captureSink, *service.RiskV2Dispatcher) {
	t.Helper()
	riskCfg := config.RiskV2Config{
		Enabled:               true,
		FingerprintHMACKey:    config.SecretString(strings.Repeat("k", 40)),
		FingerprintKeyVersion: "v1",
		MaxTextBytes:          16 * 1024,
	}
	sink := &captureSink{}
	disp := service.NewRiskV2Dispatcher(16, sink)
	disp.Start()
	gs := &service.GatewayService{}
	gs.SetRiskV2(riskCfg, disp)
	cfg := &config.Config{}
	cfg.Risk.V2 = riskCfg
	h := &GatewayHandler{gatewayService: gs, cfg: cfg}
	return h, sink, disp
}

func newForwardCtx(ua string, body []byte) *gin.Context {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("User-Agent", ua)
	c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{ID: 7, UserID: 42})
	return c
}

func forwardEnv() service.EdgeUsageEnvelope {
	return service.EdgeUsageEnvelope{
		Platform: service.PlatformAnthropic,
		Model:    "claude-3-5-sonnet",
		Stream:   true,
		Usage:    service.ClaudeUsage{InputTokens: 100, OutputTokens: 2000},
	}
}

// Claude Code(claude-cli UA)→ 产生一条转发观测。
func TestObserveForwardedRiskV2_ClaudeCode(t *testing.T) {
	h, sink, disp := newForwardRiskHandler(t)
	body := []byte(`{"model":"claude-3-5-sonnet","messages":[{"role":"user","content":"hello world"}]}`)
	c := newForwardCtx("claude-cli/1.2.3", body)

	h.ObserveForwardedRiskV2(c, forwardEnv(), body, time.Now())
	_ = disp.Stop(context.Background())

	if sink.len() != 1 {
		t.Fatalf("claude-cli request should enqueue exactly 1 observation, got %d", sink.len())
	}
}

// 反代改写过 UA(如 Go-http-client)的 Claude 转发流量 → 照样采集(不再按 UA 过滤)。
func TestObserveForwardedRiskV2_RewrittenUA_Collects(t *testing.T) {
	h, sink, disp := newForwardRiskHandler(t)
	body := []byte(`{"model":"claude-3-5-sonnet","messages":[{"role":"user","content":"hello world"}]}`)
	c := newForwardCtx("Go-http-client/2.0", body)

	h.ObserveForwardedRiskV2(c, forwardEnv(), body, time.Now())
	_ = disp.Stop(context.Background())

	if sink.len() != 1 {
		t.Fatalf("reverse-proxied Claude traffic should enqueue regardless of UA, got %d", sink.len())
	}
}

// OpenAI 平台的转发 → 不采（避免按 anthropic 误解析）。
func TestObserveForwardedRiskV2_OpenAISkip(t *testing.T) {
	h, sink, disp := newForwardRiskHandler(t)
	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`)
	env := forwardEnv()
	env.Platform = service.PlatformOpenAI
	c := newForwardCtx("Go-http-client/2.0", body)

	h.ObserveForwardedRiskV2(c, env, body, time.Now())
	_ = disp.Stop(context.Background())

	if sink.len() != 0 {
		t.Fatalf("openai forwarded must not enqueue, got %d", sink.len())
	}
}

// risk.v2 关闭 → 完全 no-op（即使是 Claude Code）。
func TestObserveForwardedRiskV2_Disabled(t *testing.T) {
	h, sink, disp := newForwardRiskHandler(t)
	h.cfg.Risk.V2.Enabled = false // 关掉总开关
	body := []byte(`{"model":"claude-3-5-sonnet","messages":[{"role":"user","content":"hello world"}]}`)
	c := newForwardCtx("claude-cli/1.2.3", body)

	h.ObserveForwardedRiskV2(c, forwardEnv(), body, time.Now())
	_ = disp.Stop(context.Background())

	if sink.len() != 0 {
		t.Fatalf("disabled risk.v2 must not enqueue, got %d", sink.len())
	}
}
