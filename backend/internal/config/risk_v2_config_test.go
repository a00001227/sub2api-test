package config

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

const testSecret = "supersecretkey_1234567890_abcdefghijklmnop" // >=32

// ─────────────────────────── SecretString 脱敏 ───────────────────────────

func TestSecretString_RedactsEverywhere(t *testing.T) {
	s := SecretString(testSecret)

	if s.Reveal() != testSecret {
		t.Fatal("Reveal must return plaintext for HMAC use")
	}
	// String / fmt 各动词均脱敏（%v %s %+v %#v）。
	for _, got := range []string{
		s.String(),
		fmt.Sprintf("%v", s),
		fmt.Sprintf("%s", s),
		fmt.Sprintf("%+v", s),
		fmt.Sprintf("%#v", s),
	} {
		if strings.Contains(got, testSecret) {
			t.Errorf("secret leaked via fmt: %q", got)
		}
		if got != secretRedacted {
			t.Errorf("expected %q, got %q", secretRedacted, got)
		}
	}
	// JSON / text 序列化脱敏。
	if b, _ := json.Marshal(s); strings.Contains(string(b), testSecret) {
		t.Errorf("secret leaked via json: %s", b)
	}
	if b, _ := s.MarshalText(); strings.Contains(string(b), testSecret) {
		t.Errorf("secret leaked via text: %s", b)
	}
}

func TestRiskV2Config_SecretNotInSerialization(t *testing.T) {
	cfg := RiskV2Config{
		Enabled:               true,
		FingerprintHMACKey:    SecretString(testSecret),
		FingerprintKeyVersion: "v1",
		QueueSize:             4096,
		MaxTextBytes:          16384,
	}
	// json:"-" + SecretString 双保险：JSON、fmt 都不得含密钥。
	blob, _ := json.Marshal(cfg)
	if strings.Contains(string(blob), testSecret) {
		t.Errorf("secret leaked in RiskV2Config JSON: %s", blob)
	}
	for _, verb := range []string{"%v", "%+v", "%#v"} {
		if s := fmt.Sprintf(verb, cfg); strings.Contains(s, testSecret) {
			t.Errorf("secret leaked in RiskV2Config fmt %s", verb)
		}
	}
	// 整个 Config 序列化也不得泄漏。
	whole := Config{}
	whole.Risk.V2 = cfg
	if b, _ := json.Marshal(whole); strings.Contains(string(b), testSecret) {
		t.Error("secret leaked in whole Config JSON")
	}
	if s := fmt.Sprintf("%+v", whole); strings.Contains(s, testSecret) {
		t.Error("secret leaked in whole Config fmt")
	}
}

// ─────────────────────────── Validate / EffectiveEnabled ───────────────────────────

func TestRiskV2Config_Validate(t *testing.T) {
	// disabled → 不要求 key
	if err := (RiskV2Config{Enabled: false}).Validate(); err != nil {
		t.Errorf("disabled must not require key: %v", err)
	}
	// enabled + 缺/短 key → err，且只含键名不含值
	const weakKey = "zzk9" // 短且不是错误信息里的任何词，用于验证错误文本不回显密钥值
	err := (RiskV2Config{Enabled: true, FingerprintHMACKey: SecretString(weakKey), FingerprintKeyVersion: "v1"}).Validate()
	if err == nil {
		t.Fatal("short key must be invalid when enabled")
	}
	if !strings.Contains(err.Error(), "risk.v2.fingerprint_hmac_key") {
		t.Errorf("error must name the config key, got %q", err.Error())
	}
	if strings.Contains(err.Error(), weakKey) {
		t.Errorf("error must NOT contain the key value")
	}
	// enabled + valid key + bad key version → err naming key version
	err = (RiskV2Config{Enabled: true, FingerprintHMACKey: SecretString(testSecret), FingerprintKeyVersion: "bad version!!"}).Validate()
	if err == nil || !strings.Contains(err.Error(), "risk.v2.fingerprint_key_version") {
		t.Errorf("bad key version must be invalid and named, got %v", err)
	}
	// enabled + valid → nil
	if err := (RiskV2Config{Enabled: true, FingerprintHMACKey: SecretString(testSecret), FingerprintKeyVersion: "v1"}).Validate(); err != nil {
		t.Errorf("valid config must pass: %v", err)
	}
}

func TestRiskV2Config_EffectiveEnabled(t *testing.T) {
	if (RiskV2Config{Enabled: false}).EffectiveEnabled() {
		t.Error("disabled → not effective")
	}
	if (RiskV2Config{Enabled: true, FingerprintHMACKey: "short", FingerprintKeyVersion: "v1"}).EffectiveEnabled() {
		t.Error("enabled+invalid → DEGRADED, not effective")
	}
	if !(RiskV2Config{Enabled: true, FingerprintHMACKey: SecretString(testSecret), FingerprintKeyVersion: "v1"}).EffectiveEnabled() {
		t.Error("enabled+valid → effective")
	}
}

// ─────────────────────────── bounds ───────────────────────────

func TestRiskV2Config_Bounds(t *testing.T) {
	cases := []struct {
		in, want int
		fn       func(RiskV2Config) int
	}{
		{0, RiskV2QueueSizeDefault, func(c RiskV2Config) int { return c.QueueSizeNormalized() }},
		{1, RiskV2QueueSizeMin, func(c RiskV2Config) int { return c.QueueSizeNormalized() }},
		{1 << 30, RiskV2QueueSizeMax, func(c RiskV2Config) int { return c.QueueSizeNormalized() }},
		{5000, 5000, func(c RiskV2Config) int { return c.QueueSizeNormalized() }},
	}
	for _, c := range cases {
		if got := c.fn(RiskV2Config{QueueSize: c.in}); got != c.want {
			t.Errorf("QueueSizeNormalized(%d)=%d want %d", c.in, got, c.want)
		}
	}
	mt := []struct{ in, want int }{
		{0, RiskV2MaxTextBytesDefault},
		{1, RiskV2MaxTextBytesMin},
		{1 << 30, RiskV2MaxTextBytesMax},
		{20000, 20000},
	}
	for _, c := range mt {
		if got := (RiskV2Config{MaxTextBytes: c.in}).MaxTextBytesNormalized(); got != c.want {
			t.Errorf("MaxTextBytesNormalized(%d)=%d want %d", c.in, got, c.want)
		}
	}
}

// ─────────────── 切片 4.2 §二：Cycle/Retry TTL 公式 ───────────────

func TestWorkerTTLFormula_Defaults(t *testing.T) {
	// 默认 interval=5m, max_catchup=3, max_retry=3, cycle_timeout=240s。
	w := RiskV2WorkerConfig{}
	// cycle_state_ttl = max(2h, 5*5m=25m, 2*240s=8m) = 2h。
	if got := w.CycleStateTTL(); got != 2*time.Hour {
		t.Errorf("default CycleStateTTL = %s, want 2h", got)
	}
	// retry_ttl = max(2h, 5*5m=25m) = 2h。
	if got := w.RetryTTL(); got != 2*time.Hour {
		t.Errorf("default RetryTTL = %s, want 2h", got)
	}
	// 必须 >= cycle_timeout。
	if w.CycleStateTTL() < time.Duration(RiskV2WorkerCycleTimeoutDefault)*time.Second {
		t.Error("CycleStateTTL must be >= cycle_timeout")
	}
	if err := w.Validate(); err != nil {
		t.Errorf("default worker config must validate: %v", err)
	}
}

func TestWorkerTTLFormula_NonDefaultInterval(t *testing.T) {
	// interval=30m → (3+2)*30m=150m=2.5h > 2h。
	w := RiskV2WorkerConfig{IntervalSeconds: 1800, CycleTimeoutSecs: 600}
	if got := w.CycleStateTTL(); got != 150*time.Minute {
		t.Errorf("CycleStateTTL = %s, want 150m", got)
	}
	if got := w.RetryTTL(); got != 150*time.Minute {
		t.Errorf("RetryTTL = %s, want 150m", got)
	}
	if err := w.Validate(); err != nil {
		t.Errorf("must validate: %v", err)
	}
}

func TestWorkerTTLFormula_LongInterval(t *testing.T) {
	// interval=1h → (3+2)*1h=5h。cycle_timeout=1800s(<1h)。
	w := RiskV2WorkerConfig{IntervalSeconds: 3600, CycleTimeoutSecs: 1800}
	if got := w.CycleStateTTL(); got != 5*time.Hour {
		t.Errorf("CycleStateTTL = %s, want 5h", got)
	}
	if got := w.RetryTTL(); got != 5*time.Hour {
		t.Errorf("RetryTTL = %s, want 5h", got)
	}
	// 恒 >= cycle_timeout。
	if w.CycleStateTTL() < time.Duration(1800)*time.Second {
		t.Error("CycleStateTTL must be >= cycle_timeout")
	}
	if err := w.Validate(); err != nil {
		t.Errorf("must validate: %v", err)
	}
}

func TestWorkerTTL_NeverBelowCycleTimeout(t *testing.T) {
	// 极端：interval 略大于 cycle_timeout。cycle_timeout 必须 < interval（否则 Validate 拒绝）。
	w := RiskV2WorkerConfig{IntervalSeconds: 4000, CycleTimeoutSecs: 3999}
	if w.CycleStateTTL() < time.Duration(3999)*time.Second {
		t.Errorf("CycleStateTTL %s must be >= cycle_timeout 3999s", w.CycleStateTTL())
	}
}

func TestServerGracefulTimeouts(t *testing.T) {
	var c ServerConfig
	if c.GracefulShutdownTimeout() != time.Duration(ServerGracefulShutdownDefaultSeconds)*time.Second {
		t.Errorf("default graceful = %s", c.GracefulShutdownTimeout())
	}
	if c.GracefulForceTimeout() != time.Duration(ServerGracefulForceDefaultSeconds)*time.Second {
		t.Errorf("default force = %s", c.GracefulForceTimeout())
	}
	// 不再是硬编码 5s。
	if c.GracefulShutdownTimeout() == 5*time.Second {
		t.Error("graceful shutdown must not default to hardcoded 5s")
	}
	c2 := ServerConfig{GracefulShutdownTimeoutSeconds: 120, GracefulForceTimeoutSeconds: 15}
	if c2.GracefulShutdownTimeout() != 120*time.Second || c2.GracefulForceTimeout() != 15*time.Second {
		t.Error("configurable graceful/force timeouts must be honored")
	}
}
