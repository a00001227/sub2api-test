package service

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/domain"
)

// ─────────────────────────── helpers ───────────────────────────

func testRiskV2Cfg() config.RiskV2Config {
	return config.RiskV2Config{
		Enabled:               true,
		FingerprintHMACKey:    config.SecretString(strings.Repeat("k", 40)),
		FingerprintKeyVersion: "v1",
		MaxTextBytes:          16 * 1024,
	}
}

func mustParse(t *testing.T, body, protocol string) *ParsedRequest {
	t.Helper()
	p, err := ParseGatewayRequest(NewRequestBodyRef([]byte(body)), protocol)
	if err != nil {
		t.Fatalf("ParseGatewayRequest(%s): %v", protocol, err)
	}
	return p
}

func exactFP(s string) string {
	return riskKeyedFingerprint(riskPurposeExact, canonicalizeExactText(s), "key", "v1")
}
func scaffoldFP(s string) string {
	return riskKeyedFingerprint(riskPurposeScaffold, scaffoldText(s), "key", "v1")
}

// ─────────────────────────── area 4：content-aware exact canonicalization ───────────────────────────

func TestCanonExact_PythonIndentation_Differs(t *testing.T) {
	if exactFP("def f():\n    return 1") == exactFP("def f():\n        return 1") {
		t.Error("Python indentation change must produce DIFFERENT exact HMAC (internal whitespace preserved)")
	}
}

func TestCanonExact_InternalSpaces_Differ(t *testing.T) {
	if exactFP("a  b") == exactFP("a b") {
		t.Error(`"a  b" vs "a b" must produce different exact HMAC`)
	}
}

func TestCanonExact_CRLFvsLF_Same(t *testing.T) {
	if exactFP("hello\r\nworld") != exactFP("hello\nworld") {
		t.Error("CRLF vs LF of same natural text must produce identical exact HMAC")
	}
}

func TestCanonExact_JSONKeyOrder_Same(t *testing.T) {
	if exactFP(`{"b":1,"a":2}`) != exactFP(`{"a":2,  "b":1}`) {
		t.Error("JSON key order/whitespace must canonicalize to same exact HMAC")
	}
}

func TestCanonExact_JSONStringInternalWhitespace_Differs(t *testing.T) {
	if exactFP(`{"a":"x  y"}`) == exactFP(`{"a":"x y"}`) {
		t.Error("JSON string-internal whitespace must be preserved → different exact HMAC")
	}
}

func TestCanonExact_YAMLIndentation_Differs(t *testing.T) {
	if exactFP("root:\n  a: 1") == exactFP("root:\n      a: 1") {
		t.Error("YAML indentation change must produce different exact HMAC")
	}
}

func TestCanonExact_LargeIntPreserved(t *testing.T) {
	if exactFP(`{"n":100200300400500}`) == exactFP(`{"n":100200300400501}`) {
		t.Error("large integer scalar must be preserved exactly (UseNumber)")
	}
}

func TestNearDuplicate_ToleratesFormatting(t *testing.T) {
	// near_duplicate 宽松：空白/大小写变化仍视作近重复（相同 sketch）。
	a := ComputeMessagesSimhash([]byte(nearDuplicateText("Hello   World\r\nFoo")))
	b := ComputeMessagesSimhash([]byte(nearDuplicateText("hello world foo")))
	if a != b {
		t.Error("near_duplicate should fold formatting/case to the same sketch")
	}
}

// ─────────────────────────── area 2/7：exact 语义 + 变量 + purpose separation ───────────────────────────

func TestExact_DifferentUUID_DifferentHMAC(t *testing.T) {
	if exactFP("id 550e8400-e29b-41d4-a716-446655440000 x") == exactFP("id 550e8400-e29b-41d4-a716-999999999999 x") {
		t.Error("different UUID → different exact HMAC")
	}
}

func TestScaffold_DifferentUUID_SameScaffold(t *testing.T) {
	if scaffoldFP("id 550e8400-e29b-41d4-a716-446655440000 x") != scaffoldFP("id 550e8400-e29b-41d4-a716-999999999999 x") {
		t.Error("different UUID → same scaffold fingerprint")
	}
}

func TestHMAC_PurposeSeparation(t *testing.T) {
	// 即便文本与密钥完全相同，exact 与 scaffold 用途域分隔后指纹也必须不同。
	same := "identical text"
	e := riskKeyedFingerprint(riskPurposeExact, same, "key", "v1")
	s := riskKeyedFingerprint(riskPurposeScaffold, same, "key", "v1")
	if e == s {
		t.Error("purpose separation must make exact vs scaffold HMAC differ for identical input")
	}
}

// ─────────────────────────── area 2/3：截断输入下 exact 不可用 + 有界 ───────────────────────────

func longBody(turn string) string {
	b, _ := json.Marshal(turn)
	return `{"model":"claude","messages":[{"role":"user","content":` + string(b) + `}]}`
}

func TestFeatures_Truncated_ExactUnavailable_ScaffoldNearSampled(t *testing.T) {
	cfg := testRiskV2Cfg()
	cfg.MaxTextBytes = 1024
	big := strings.Repeat("alpha beta ", 5000) // >> 1KB
	f := BuildRiskV2RequestFeatures(mustParse(t, longBody(big), domain.PlatformAnthropic), cfg)
	if f == nil || !f.TurnAvailable {
		t.Fatal("expected features")
	}
	if !f.InputTruncated {
		t.Fatal("expected InputTruncated=true")
	}
	if f.ExactFingerprintAvailable || f.ExactHMAC != "" {
		t.Error("truncated input must NOT have exact fingerprint available")
	}
	if f.ScaffoldHMAC == "" || !f.ScaffoldFingerprintSampled {
		t.Error("scaffold must be computed on sample and marked sampled")
	}
	if f.NearDupSimhash == 0 || !f.NearFingerprintSampled {
		t.Error("near-dup must be computed on sample and marked sampled")
	}
}

func TestFeatures_TwoTruncated_NotExactMatched(t *testing.T) {
	cfg := testRiskV2Cfg()
	cfg.MaxTextBytes = 1024
	a := BuildRiskV2RequestFeatures(mustParse(t, longBody(strings.Repeat("x", 9000)+"AAA"+strings.Repeat("y", 9000)), domain.PlatformAnthropic), cfg)
	b := BuildRiskV2RequestFeatures(mustParse(t, longBody(strings.Repeat("x", 9000)+"BBB"+strings.Repeat("y", 9000)), domain.PlatformAnthropic), cfg)
	// 两个长 prompt 只在中段不同：exact 均不可用 → 绝不可能被判为 Exact Match。
	if a.ExactFingerprintAvailable || b.ExactFingerprintAvailable {
		t.Error("truncated prompts must never expose an exact fingerprint")
	}
	if a.ExactHMAC != "" || b.ExactHMAC != "" {
		t.Error("truncated prompts must not carry a (fake) exact HMAC to aggregate on")
	}
}

func TestFeatures_NotTruncated_ExactAvailable(t *testing.T) {
	cfg := testRiskV2Cfg()
	f := BuildRiskV2RequestFeatures(mustParse(t, longBody("short and complete turn"), domain.PlatformAnthropic), cfg)
	if f == nil || f.InputTruncated {
		t.Fatal("expected non-truncated")
	}
	if !f.ExactFingerprintAvailable || f.ExactHMAC == "" {
		t.Error("complete input must have exact fingerprint available")
	}
}

func TestFeatures_ShapesAndGating(t *testing.T) {
	cfg := testRiskV2Cfg()
	for _, tc := range []struct{ name, body, proto string }{
		{"anthropic", `{"model":"claude","messages":[{"role":"user","content":"hi anthropic"}]}`, domain.PlatformAnthropic},
		{"openai", `{"model":"gpt","messages":[{"role":"user","content":"hi openai"}]}`, ""},
		{"responses", `{"model":"gpt","input":"hi responses"}`, "responses"},
	} {
		f := BuildRiskV2RequestFeatures(mustParse(t, tc.body, tc.proto), cfg)
		if f == nil || !f.TurnAvailable || f.ExactHMAC == "" {
			t.Errorf("%s: expected complete features, got %+v", tc.name, f)
		}
	}
	if BuildRiskV2RequestFeatures(mustParse(t, `{"messages":[{"role":"user","content":"x"}]}`, domain.PlatformAnthropic), config.RiskV2Config{Enabled: false}) != nil {
		t.Error("disabled → nil")
	}
	degraded := config.RiskV2Config{Enabled: true, FingerprintHMACKey: "short", FingerprintKeyVersion: "v1", MaxTextBytes: 4096}
	if BuildRiskV2RequestFeatures(mustParse(t, `{"messages":[{"role":"user","content":"x"}]}`, domain.PlatformAnthropic), degraded) != nil {
		t.Error("DEGRADED → nil (no empty HMAC)")
	}
}

// ─────────────────────────── 分段 edge cases ───────────────────────────

func TestSegment_ToolResultOnly_NoFallback(t *testing.T) {
	raw := `[{"role":"user","content":"OLD_HISTORY"},{"role":"assistant","content":"a"},{"role":"user","content":[{"type":"tool_result","content":"TOOL_OUT"}]}]`
	seg := segmentRiskFromRaw([]byte(raw), nil, nil, false, 4096)
	if strings.Contains(seg.NewUserTurn, "OLD_HISTORY") || strings.Contains(seg.NewUserTurn, "TOOL_OUT") {
		t.Fatalf("tool_result-only turn must not fall back to history nor include tool output; got %q", seg.NewUserTurn)
	}
	if !seg.ToolResultPresent {
		t.Error("ToolResultPresent expected")
	}
}

func TestSegment_ImageOnly_NoFallback(t *testing.T) {
	raw := `[{"role":"user","content":"OLD_HISTORY"},{"role":"user","content":[{"type":"image","source":{"type":"base64","data":"QUJDQUJD"}}]}]`
	seg := segmentRiskFromRaw([]byte(raw), nil, nil, false, 4096)
	if strings.Contains(seg.NewUserTurn, "OLD_HISTORY") || strings.Contains(seg.NewUserTurn, "QUJDQUJD") {
		t.Fatalf("image-only turn must not fall back nor include base64; got %q", seg.NewUserTurn)
	}
}

func TestSegment_AssistantPrefill(t *testing.T) {
	raw := `[{"role":"user","content":"THE_TURN"},{"role":"assistant","content":"prefill"}]`
	seg := segmentRiskFromRaw([]byte(raw), nil, nil, false, 4096)
	if !strings.Contains(seg.NewUserTurn, "THE_TURN") || strings.Contains(seg.NewUserTurn, "prefill") {
		t.Errorf("assistant prefill: turn must be last user; got %q", seg.NewUserTurn)
	}
}

// ─────────────────────────── area 1：server_request_id 去重语义（service seam）───────────────────────────

func newRiskService(t *testing.T, cfg config.RiskV2Config, sink RiskV2Sink) (*GatewayService, *RiskV2Dispatcher) {
	t.Helper()
	d := NewRiskV2Dispatcher(64, sink)
	d.Start()
	s := &GatewayService{}
	s.SetRiskV2(cfg, d)
	return s, d
}

func feat(server string) RiskUsageFeatures {
	return RiskUsageFeatures{
		ServerRequestID: server,
		V2Request:       &RiskV2RequestFeatures{TurnAvailable: true, ExactFingerprintAvailable: true, ExactHMAC: "v1:x"},
	}
}

func TestDedup_DifferentServer_TwoObservations(t *testing.T) {
	rec := &recordingSink{}
	s, d := newRiskService(t, testRiskV2Cfg(), rec)
	// 两个不同下游请求（不同 server id）即使 billing/client 相同 → 两条观测。
	s.enqueueRiskV2(&UsageLog{UserID: 1, APIKeyID: 2, RequestID: "billing", InputTokens: 1}, feat("server-A"), "anthropic")
	s.enqueueRiskV2(&UsageLog{UserID: 1, APIKeyID: 2, RequestID: "billing", InputTokens: 1}, feat("server-B"), "anthropic")
	_ = d.Stop(context.Background())
	if rec.len() != 2 {
		t.Fatalf("different server ids → 2 observations, got %d", rec.len())
	}
}

func TestDedup_SameServer_MultipleCallbacks_OneObservation(t *testing.T) {
	rec := &recordingSink{}
	s, d := newRiskService(t, testRiskV2Cfg(), rec)
	// 同一 server id 的多次 usage 回调（含上游重试重复记账）+ 终态 → 只一条。
	s.enqueueRiskV2(&UsageLog{UserID: 1, RequestID: "b1", InputTokens: 1}, feat("server-X"), "anthropic")
	s.enqueueRiskV2(&UsageLog{UserID: 1, RequestID: "b2", InputTokens: 1}, feat("server-X"), "anthropic")
	s.EnqueueRiskV2Terminal("server-X", 1, 2, time.Now(), "terminal_error")
	_ = d.Stop(context.Background())
	if rec.len() != 1 {
		t.Fatalf("same server id → exactly one observation, got %d", rec.len())
	}
}

func TestDedup_ServerAlwaysValid(t *testing.T) {
	rec := &recordingSink{}
	s, d := newRiskService(t, testRiskV2Cfg(), rec)
	s.enqueueRiskV2(&UsageLog{UserID: 1, RequestID: "", InputTokens: 1}, feat("server-solo"), "anthropic")
	_ = d.Stop(context.Background())
	if rec.len() != 1 {
		t.Fatalf("server id must produce one observation regardless of client, got %d", rec.len())
	}
}

func TestDedup_ManyDistinctServer_AllCounted(t *testing.T) {
	rec := &recordingSink{}
	s, d := newRiskService(t, testRiskV2Cfg(), rec)
	// 恶意固定 client（已不入 envelope）不影响计数；每个下游请求有唯一 server id。
	for i := 0; i < 20; i++ {
		s.enqueueRiskV2(&UsageLog{UserID: 1, RequestID: "same-billing", InputTokens: 1}, feat(fmt.Sprintf("server-%d", i)), "anthropic")
	}
	_ = d.Stop(context.Background())
	if rec.len() != 20 {
		t.Fatalf("distinct server ids must all be counted; want 20, got %d", rec.len())
	}
}

// ─────────────────────────── area 5：cache applicability / presence（seam）───────────────────────────

func TestCachePresence_ApplicableSuccessVsTerminalVsNonApplicable(t *testing.T) {
	rec := &recordingSink{}
	s, d := newRiskService(t, testRiskV2Cfg(), rec)
	// Applicable(anthropic)+成功：显式 0 → ptr(0)，>0 → ptr(value)。
	s.enqueueRiskV2(&UsageLog{UserID: 1, RequestID: "b", InputTokens: 3, OutputTokens: 4, CacheCreationTokens: 0, CacheReadTokens: 512}, feat("srv-ok"), "anthropic")
	// 非 Applicable(openai)：Applicable=false、Available=false、指针 nil（不填 0）。
	s.enqueueRiskV2(&UsageLog{UserID: 1, RequestID: "b2", InputTokens: 3, OutputTokens: 4, CacheReadTokens: 999}, feat("srv-openai"), "openai")
	// 终态：无最终 usage → Applicable/Available 均 false，指针 nil。
	s.EnqueueRiskV2Terminal("srv-term", 1, 2, time.Now(), "terminal_status_500")
	_ = d.Stop(context.Background())

	m := map[string]RiskFeatureEnvelope{}
	for _, e := range rec.snapshot() {
		m[e.ServerRequestID] = e
	}
	ok := m["srv-ok"]
	if !ok.CacheUsageApplicable || !ok.CacheUsageAvailable || ok.CacheCreationTokens == nil || *ok.CacheCreationTokens != 0 || ok.CacheReadTokens == nil || *ok.CacheReadTokens != 512 {
		t.Errorf("applicable success: 0→ptr(0), value→ptr, applicable+available: %+v", ok)
	}
	oa := m["srv-openai"]
	if oa.CacheUsageApplicable || oa.CacheUsageAvailable || oa.CacheCreationTokens != nil || oa.CacheReadTokens != nil {
		t.Errorf("non-applicable provider: applicable/available false + nil pointers (not 0): %+v", oa)
	}
	term := m["srv-term"]
	if term.CacheUsageApplicable || term.CacheUsageAvailable || term.CacheCreationTokens != nil || term.CacheReadTokens != nil {
		t.Errorf("terminal: applicable/available false + nil pointers: %+v", term)
	}
}

// 真实解析路径：ClaudeUsage 用 int 字段（缺失→0），故 presence 由 CacheUsageAvailable 在 envelope 层表达。
func TestClaudeUsageParse_CachePresenceViaAvailability(t *testing.T) {
	var u ClaudeUsage
	// non-streaming：只返回 cache_read（cache_creation 缺失→0）。
	if err := json.Unmarshal([]byte(`{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":512}`), &u); err != nil {
		t.Fatal(err)
	}
	if u.CacheReadInputTokens != 512 || u.CacheCreationInputTokens != 0 {
		t.Errorf("parse mismatch: %+v", u)
	}
	// 结论：int 层无法区分「缺失」与「显式 0」；Risk V2 用 CacheUsageAvailable（是否拿到最终 usage）
	// 表达 presence——成功=可读，终态/取消/无 usage=nil。上面的 seam 测试已覆盖。
}

// ─────────────────────────── area 6：privacy sentinel + 白名单 ───────────────────────────

const riskSentinel = "ZZQX_UNIQUE_SENTINEL_9137"

func TestPrivacy_SentinelNeverLeaks(t *testing.T) {
	cfg := testRiskV2Cfg()
	f := BuildRiskV2RequestFeatures(mustParse(t, longBody("secret "+riskSentinel+" here"), domain.PlatformAnthropic), cfg)
	if f == nil {
		t.Fatal("features")
	}
	rec := &recordingSink{}
	s, d := newRiskService(t, cfg, rec)
	s.enqueueRiskV2(&UsageLog{UserID: 1, APIKeyID: 2, RequestID: "b", InputTokens: 1}, RiskUsageFeatures{ServerRequestID: "srv", V2Request: f}, "anthropic")
	_ = d.Stop(context.Background())
	env, _ := rec.last()
	blob, _ := json.Marshal(env)
	for i, surface := range []string{fmt.Sprintf("%+v", env), fmt.Sprintf("%#v", env), string(blob), fmt.Sprintf("%+v", *f), fmt.Sprintf("%+v", d.Stats())} {
		if strings.Contains(surface, riskSentinel) {
			t.Fatalf("sentinel leaked in surface #%d", i)
		}
	}
}

func TestPrivacy_FieldWhitelist(t *testing.T) {
	envAllowed := map[string]bool{
		"ServerRequestID": true, "UserID": true, "APIKeyID": true,
		"RequestStartedAt": true, "RequestCompletedAt": true, "TerminalStatus": true, "UsageAvailable": true,
		"Model": true, "Streaming": true, "MaxTokens": true, "Temperature": true, "InputTokens": true, "OutputTokens": true,
		"TopP": true, "HasSeed": true, "ResponseFormatType": true,
		"CacheUsageApplicable": true, "CacheUsageAvailable": true, "CacheCreationTokens": true, "CacheReadTokens": true,
		"TurnAvailable": true, "InputOriginalBytes": true, "InputSampledBytes": true, "InputTruncated": true,
		"ExactFingerprintAvailable": true, "ExactHMAC": true, "ScaffoldHMAC": true, "ScaffoldFingerprintSampled": true,
		"NearDupSimhash": true, "HasNearDup": true, "NearFingerprintSampled": true,
		"KeyVersion": true, "FingerprintVersion": true, "NormalizationVersion": true,
		"SystemPresent": true, "ToolsPresent": true, "ToolResultPresent": true, "HistoryCount": true,
	}
	featAllowed := map[string]bool{
		"TurnAvailable": true, "InputOriginalBytes": true, "InputSampledBytes": true, "InputTruncated": true,
		"ExactFingerprintAvailable": true, "ExactHMAC": true, "ScaffoldHMAC": true, "ScaffoldFingerprintSampled": true,
		"NearDupSimhash": true, "HasNearDup": true, "NearFingerprintSampled": true,
		"KeyVersion": true, "FingerprintVersion": true, "NormalizationVersion": true,
		"SystemPresent": true, "ToolsPresent": true, "ToolResultPresent": true, "HistoryCount": true, "Streaming": true,
		"Temperature": true, "TopP": true, "MaxTokens": true, "HasSeed": true, "ResponseFormatType": true,
	}
	check := func(typ reflect.Type, allowed map[string]bool) {
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			if !allowed[f.Name] {
				t.Errorf("%s.%s not in privacy whitelist — review before adding", typ.Name(), f.Name)
			}
			if f.Type.Kind() == reflect.Slice && f.Type.Elem().Kind() == reflect.Uint8 {
				t.Errorf("%s.%s is []byte — risk structs must never carry raw bytes", typ.Name(), f.Name)
			}
		}
	}
	check(reflect.TypeOf(RiskFeatureEnvelope{}), envAllowed)
	check(reflect.TypeOf(RiskV2RequestFeatures{}), featAllowed)
}

// ─────────────────────────── area 3：bounded benchmarks ───────────────────────────

func benchFeatures(b *testing.B, turnBytes int, cfg config.RiskV2Config) {
	body := longBody(strings.Repeat("a", turnBytes))
	p, err := ParseGatewayRequest(NewRequestBodyRef([]byte(body)), domain.PlatformAnthropic)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = BuildRiskV2RequestFeatures(p, cfg)
	}
}

func cfg8k() config.RiskV2Config { c := testRiskV2Cfg(); c.MaxTextBytes = 8 * 1024; return c }

func BenchmarkRiskV2_64KB_max8K(b *testing.B) { benchFeatures(b, 64*1024, cfg8k()) }
func BenchmarkRiskV2_1MB_max8K(b *testing.B)  { benchFeatures(b, 1024*1024, cfg8k()) }
func BenchmarkRiskV2_8MB_max8K(b *testing.B)  { benchFeatures(b, 8*1024*1024, cfg8k()) }

func BenchmarkRiskV2_MultiBlock_max8K(b *testing.B) {
	var blocks []string
	for i := 0; i < 256; i++ {
		blocks = append(blocks, `{"type":"text","text":"`+strings.Repeat("blk ", 64)+`"}`)
	}
	body := `{"model":"claude","messages":[{"role":"user","content":[` + strings.Join(blocks, ",") + `]}]}`
	p, _ := ParseGatewayRequest(NewRequestBodyRef([]byte(body)), domain.PlatformAnthropic)
	c := cfg8k()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = BuildRiskV2RequestFeatures(p, c)
	}
}

func BenchmarkRiskV2_SingleHugeBlock_max8K(b *testing.B) { benchFeatures(b, 8*1024*1024, cfg8k()) }
