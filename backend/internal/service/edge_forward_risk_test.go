package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/domain"
)

// buildForwardedFeat 造一份带 V2Request 的请求侧特征(模拟 handler 从转发请求体算出的)。
func buildForwardedFeat(t *testing.T) RiskUsageFeatures {
	t.Helper()
	v2 := BuildRiskV2RequestFeatures(
		mustParse(t, `{"model":"claude-3-5-sonnet","messages":[{"role":"user","content":"hello world"}]}`, domain.PlatformAnthropic),
		testRiskV2Cfg(),
	)
	if v2 == nil {
		t.Fatal("expected non-nil V2Request from valid cfg + parsed body")
	}
	return RiskUsageFeatures{V2Request: v2, ServerRequestID: "srv-fwd-1"}
}

func forwardEnv() EdgeUsageEnvelope {
	return EdgeUsageEnvelope{
		Platform: PlatformAnthropic,
		Model:    "claude-3-5-sonnet",
		Stream:   true,
		Usage:    ClaudeUsage{InputTokens: 100, OutputTokens: 2000, CacheReadInputTokens: 50, CacheCreationInputTokens: 10},
	}
}

// 成功路径：EffectiveEnabled + 非 nil V2Request → 入队一条，且 user/apikey/token/model 取自 env+apiKey。
func TestEnqueueForwardedRiskV2_Success(t *testing.T) {
	sink := &recordingSink{}
	d := NewRiskV2Dispatcher(16, sink)
	d.Start()
	s := &GatewayService{}
	s.SetRiskV2(testRiskV2Cfg(), d)

	s.EnqueueForwardedRiskV2(&APIKey{ID: 7, UserID: 42}, forwardEnv(), buildForwardedFeat(t))
	if err := d.Stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}

	got, n := sink.last()
	if n != 1 {
		t.Fatalf("want 1 enqueued observation, got %d", n)
	}
	if got.UserID != 42 || got.APIKeyID != 7 {
		t.Errorf("identity mismatch: user=%d apikey=%d", got.UserID, got.APIKeyID)
	}
	if got.InputTokens != 100 || got.OutputTokens != 2000 {
		t.Errorf("token mismatch: in=%d out=%d", got.InputTokens, got.OutputTokens)
	}
	if got.Model != "claude-3-5-sonnet" {
		t.Errorf("model mismatch: %q", got.Model)
	}
	if got.ServerRequestID != "srv-fwd-1" {
		t.Errorf("server_request_id mismatch: %q", got.ServerRequestID)
	}
	// anthropic 平台 → cache applicable，值取自 env。
	if !got.CacheUsageApplicable || got.CacheReadTokens == nil || *got.CacheReadTokens != 50 {
		t.Errorf("cache read mismatch: applicable=%v ptr=%v", got.CacheUsageApplicable, got.CacheReadTokens)
	}
}

// 守卫：关闭 / nil V2Request / nil apiKey / 无 dispatcher → 一律不入队。
func TestEnqueueForwardedRiskV2_Guards(t *testing.T) {
	newSvc := func(cfg config.RiskV2Config, withDisp bool) (*GatewayService, *recordingSink, *RiskV2Dispatcher) {
		sink := &recordingSink{}
		var d *RiskV2Dispatcher
		s := &GatewayService{}
		if withDisp {
			d = NewRiskV2Dispatcher(16, sink)
			d.Start()
			s.SetRiskV2(cfg, d)
		} else {
			s.riskV2Cfg = cfg // dispatcher 保持 nil
		}
		return s, sink, d
	}
	drain := func(d *RiskV2Dispatcher) {
		if d != nil {
			_ = d.Stop(context.Background())
		}
	}

	// a) risk.v2 关闭。
	s, sink, d := newSvc(config.RiskV2Config{Enabled: false}, true)
	s.EnqueueForwardedRiskV2(&APIKey{ID: 1, UserID: 1}, forwardEnv(), buildForwardedFeat(t))
	drain(d)
	if sink.len() != 0 {
		t.Errorf("disabled cfg must not enqueue, got %d", sink.len())
	}

	// b) V2Request 为 nil（本地也是这么被守卫挡掉的）。
	s, sink, d = newSvc(testRiskV2Cfg(), true)
	s.EnqueueForwardedRiskV2(&APIKey{ID: 1, UserID: 1}, forwardEnv(), RiskUsageFeatures{ServerRequestID: "x"})
	drain(d)
	if sink.len() != 0 {
		t.Errorf("nil V2Request must not enqueue, got %d", sink.len())
	}

	// c) apiKey 为 nil。
	s, sink, d = newSvc(testRiskV2Cfg(), true)
	s.EnqueueForwardedRiskV2(nil, forwardEnv(), buildForwardedFeat(t))
	drain(d)
	if sink.len() != 0 {
		t.Errorf("nil apiKey must not enqueue, got %d", sink.len())
	}

	// d) 无 dispatcher（未 SetRiskV2）→ 不 panic、不入队。
	s, sink, _ = newSvc(testRiskV2Cfg(), false)
	s.EnqueueForwardedRiskV2(&APIKey{ID: 1, UserID: 1}, forwardEnv(), buildForwardedFeat(t))
	if sink.len() != 0 {
		t.Errorf("nil dispatcher must not enqueue, got %d", sink.len())
	}
}
