package handler

import (
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

func hCfgEnabled() *config.Config {
	c := &config.Config{}
	c.Risk.V2 = config.RiskV2Config{
		Enabled:               true,
		FingerprintHMACKey:    config.SecretString(strings.Repeat("k", 40)),
		FingerprintKeyVersion: "v1",
		MaxTextBytes:          16 * 1024,
	}
	return c
}

func mustParseH(t *testing.T, body, proto string) *service.ParsedRequest {
	t.Helper()
	p, err := service.ParseGatewayRequest(service.NewRequestBodyRef([]byte(body)), proto)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return p
}

// buildRiskUsageFeatures 是 4 个 gateway handler 记账点共用的真实函数：
// 验证它把 request_started_at 透传，并在 EffectiveEnabled 时产出 V2 请求侧特征。
func TestBuildRiskUsageFeatures_ThreadsStartServerIDAndBuildsV2(t *testing.T) {
	start := time.Unix(4242, 0)
	body := `{"model":"claude","messages":[{"role":"user","content":"integration turn"}]}`
	feat := buildRiskUsageFeatures(mustParseH(t, body, domain.PlatformAnthropic), hCfgEnabled(), start, "server-req-1")

	if !feat.RequestStartedAt.Equal(start) {
		t.Errorf("request_started_at must be threaded from handler entry, got %v", feat.RequestStartedAt)
	}
	if feat.ServerRequestID != "server-req-1" {
		t.Errorf("server_request_id must be threaded, got %q", feat.ServerRequestID)
	}
	if feat.V2Request == nil || !feat.V2Request.TurnAvailable || feat.V2Request.ExactHMAC == "" {
		t.Errorf("expected V2 request features with exact HMAC, got %+v", feat.V2Request)
	}
}

func TestBuildRiskUsageFeatures_DisabledNoV2ButStartKept(t *testing.T) {
	start := time.Unix(99, 0)
	body := `{"model":"claude","messages":[{"role":"user","content":"x"}]}`
	feat := buildRiskUsageFeatures(mustParseH(t, body, domain.PlatformAnthropic), &config.Config{}, start, "srv")
	if feat.V2Request != nil {
		t.Error("disabled cfg must not build V2 features")
	}
	if !feat.RequestStartedAt.Equal(start) || feat.ServerRequestID != "srv" {
		t.Error("start + server id must be threaded even when V2 disabled")
	}
}

func TestBuildRiskUsageFeatures_NilParsedKeepsStart(t *testing.T) {
	start := time.Unix(7, 0)
	feat := buildRiskUsageFeatures(nil, hCfgEnabled(), start, "srv2")
	if !feat.RequestStartedAt.Equal(start) || feat.ServerRequestID != "srv2" {
		t.Error("nil parsed must still carry start time + server id")
	}
	if feat.V2Request != nil {
		t.Error("nil parsed must not build V2 features")
	}
}
