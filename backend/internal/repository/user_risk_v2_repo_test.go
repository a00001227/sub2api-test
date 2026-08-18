package repository

import (
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func sampleAssessment() service.RiskV2Assessment {
	return service.RiskV2Assessment{
		RiskIndex: 82, RiskTier: "HIGH", Confidence: 0.7, DataSufficient: true,
		Automation: service.RiskV2PlaneScore{Score: 90, Available: true},
		Harvest:    service.RiskV2PlaneScore{Score: 75, Available: true},
		Campaign:   service.RiskV2PlaneScore{Available: false}, // 不可用 → 应存 NULL
		Exposure:   service.RiskV2PlaneScore{Score: 30, Available: true},
		EvidenceFamilies: []service.RiskV2EvidenceFamily{
			{Family: "TEMPLATE_ENUMERATION", Group: "PROMPT_PATTERN", Available: true, Strength: 100, MeetsHigh: true, Window: "1h"},
		},
		EvidenceGroups: []service.RiskV2EvidenceGroup{{Group: "PROMPT_PATTERN", MetHigh: true, Strength: 100}},
		ReasonCodes: []service.RiskV2ReasonCode{
			{Code: "EXACT_DUPLICATION_HIGH", Window: "1h", ObservedValue: 0.9, Threshold: 0.6, EvidenceFamily: "TEMPLATE_ENUMERATION", EvidenceGroup: "PROMPT_PATTERN"},
		},
		FeatureAvailability: service.RiskV2FeatureAvailability{Requests: true, Fingerprint: true, ToolUse: true},
		Degraded:            false, Incomplete: true, IncompleteReasons: []string{"exact_incomplete:1h"},
		HealthAvailable: true,
		FeatureVersion:  "score-v2", PolicyVersion: "shadow-uncalibrated-1", FingerprintKeyVersion: "v1",
		AssessedAtUnix: 1700, EffectiveAction: "NONE",
	}
}

func TestWhitelistMarshalRoundTrip(t *testing.T) {
	a := sampleAssessment()
	fa, ef, eg, rc, err := marshalWhitelist(a)
	if err != nil {
		t.Fatal(err)
	}
	// 不得含任何敏感/内部原文标记（注意 PROMPT_PATTERN/RESPONSE_HARVEST 是合法枚举值，
	// 故不扫描 prompt/response 这类会误伤的词；只扫真正敏感的标记）。
	for _, blob := range [][]byte{fa, ef, eg, rc} {
		s := strings.ToLower(string(blob))
		for _, bad := range []string{"hmac", "simhash", "request_id", "requestid", "api_key", "apikey", "secret", "raw_prompt", "raw_response", "clientrequest", "serverrequest", "tool_schema"} {
			if strings.Contains(s, bad) {
				t.Errorf("whitelist JSON leaked forbidden token %q in %s", bad, s)
			}
		}
	}
	// 结构键存在。
	if !strings.Contains(string(ef), "\"family\"") || !strings.Contains(string(eg), "\"group\"") || !strings.Contains(string(rc), "\"code\"") {
		t.Error("whitelist JSON missing expected keys")
	}
}

func TestPlaneNullMapping(t *testing.T) {
	if n := planeNull(service.RiskV2PlaneScore{Score: 5, Available: false}); n.Valid {
		t.Error("unavailable plane must map to NULL (not 0)")
	}
	if n := planeNull(service.RiskV2PlaneScore{Score: 0, Available: true}); !n.Valid || n.Float64 != 0 {
		t.Error("available plane with score 0 must map to non-NULL 0")
	}
	if nullFPtr(planeNull(service.RiskV2PlaneScore{Available: false})) != nil {
		t.Error("nullFPtr of unavailable must be nil")
	}
}

func TestListQueryBuilder(t *testing.T) {
	tru := true
	minIdx := 40.0
	q, args := buildRiskV2ListQuery(service.RiskV2ListFilter{
		Tier: "HIGH", MinRiskIndex: &minIdx, DataSufficient: &tru, UserID: 7,
		AssessedFromUnix: 100, AssessedToUnix: 200,
	}, service.RiskV2Pagination{Limit: 10, Offset: 5})

	for _, want := range []string{"risk_tier = $1", "risk_index >= $2", "data_sufficient = $3", "user_id = $4", "assessed_at >= $5", "assessed_at <= $6", "ORDER BY risk_index DESC, assessed_at DESC, user_id ASC", "LIMIT $7 OFFSET $8"} {
		if !strings.Contains(q, want) {
			t.Errorf("query missing %q\nquery=%s", want, q)
		}
	}
	if len(args) != 8 || args[6] != 10 || args[7] != 5 {
		t.Errorf("args mismatch: %v", args)
	}
}

func TestListQueryLimitCappedAndDefaulted(t *testing.T) {
	// 超上限 → 夹到 100（禁止无界）。
	_, args := buildRiskV2ListQuery(service.RiskV2ListFilter{}, service.RiskV2Pagination{Limit: 100000})
	if args[len(args)-2] != 100 {
		t.Errorf("over-limit must cap to 100, got %v", args[len(args)-2])
	}
	// 0 → 默认 100。
	_, args2 := buildRiskV2ListQuery(service.RiskV2ListFilter{}, service.RiskV2Pagination{})
	if args2[len(args2)-2] != 100 {
		t.Errorf("zero limit must default to 100, got %v", args2[len(args2)-2])
	}
}
