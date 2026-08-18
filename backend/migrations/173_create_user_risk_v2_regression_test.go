package migrations

import (
	"strings"
	"testing"
)

// 静态校验 173 迁移：additive、幂等、不动 legacy user_risk、无敏感列、plane 分数可空。
func TestMigration173CreateUserRiskV2Additive(t *testing.T) {
	content, err := FS.ReadFile("173_create_user_risk_v2.sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	s := string(content)
	// 剥离 SQL 注释行（隐私说明会提到这些词），只对真正的 DDL 扫描禁止列。
	var ddlLines []string
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		ddlLines = append(ddlLines, line)
	}
	lower := strings.ToLower(strings.Join(ddlLines, "\n"))

	if !strings.Contains(s, "CREATE TABLE IF NOT EXISTS user_risk_v2") {
		t.Error("must create user_risk_v2 idempotently (IF NOT EXISTS)")
	}
	for _, idx := range []string{
		"idx_user_risk_v2_tier_index", "idx_user_risk_v2_assessed_at",
		"idx_user_risk_v2_degraded", "idx_user_risk_v2_updated_at",
	} {
		if !strings.Contains(s, idx) {
			t.Errorf("missing index %s", idx)
		}
	}
	if !strings.Contains(s, "CREATE INDEX IF NOT EXISTS") {
		t.Error("indexes must be created idempotently")
	}

	// additive：不得修改/删除 legacy user_risk。
	if strings.Contains(lower, "alter table user_risk") {
		t.Error("must not ALTER legacy user_risk")
	}
	if strings.Contains(lower, "drop table user_risk;") || strings.Contains(lower, "drop table if exists user_risk;") {
		t.Error("must not DROP legacy user_risk")
	}

	// 无敏感/原文列。
	for _, bad := range []string{"prompt", "response", "hmac", "simhash", "client_request", "server_request", "api_key", "tool_schema", "request_body"} {
		if strings.Contains(lower, bad) {
			t.Errorf("forbidden column/token in schema: %q", bad)
		}
	}

	// plane 分数列本身必须可空（列定义不得声明 NOT NULL；CHECK 里的 IS NOT NULL 不算）。
	for _, col := range []string{"automation_score", "harvest_score", "campaign_score", "exposure_score"} {
		if strings.Contains(lower, col+" double precision not null") {
			t.Errorf("plane score %s column must be nullable (no NOT NULL on column def)", col)
		}
	}

	// 无 GIN 索引（禁止高成本 JSONB GIN）。
	if strings.Contains(lower, "using gin") {
		t.Error("must not create JSONB GIN index")
	}

	// 约束/FK 存在（3.1 加固）。
	if !strings.Contains(lower, "references users(id) on delete cascade") {
		t.Error("must have FK to users(id) ON DELETE CASCADE")
	}
	if !strings.Contains(lower, "risk_tier in ('insufficient_data','watch','medium','high')") {
		t.Error("must CHECK risk_tier enum")
	}
	if !strings.Contains(lower, "effective_action = 'none'") {
		t.Error("must CHECK effective_action = 'NONE'")
	}
	if !strings.Contains(lower, "confidence >= 0 and confidence <= 1") {
		t.Error("must CHECK confidence in [0,1]")
	}
	if !strings.Contains(lower, "risk_index >= 0 and risk_index <= 100") {
		t.Error("must CHECK risk_index in [0,100]")
	}
	// available/score 一致性约束存在。
	for _, c := range []string{"chk_user_risk_v2_automation", "chk_user_risk_v2_harvest", "chk_user_risk_v2_campaign", "chk_user_risk_v2_exposure"} {
		if !strings.Contains(lower, c) {
			t.Errorf("missing available/score consistency constraint %s", c)
		}
	}
	// assessment_digest 列存在，且为定长 64 + NOT NULL + 小写 hex 正则 CHECK（无 DEFAULT 兜底，写入方必须给合法摘要）。
	if !strings.Contains(lower, "assessment_digest") {
		t.Error("must have assessment_digest column")
	}
	if !strings.Contains(lower, "assessment_digest       char(64)    not null") &&
		!strings.Contains(lower, "assessment_digest char(64) not null") {
		t.Error("assessment_digest must be CHAR(64) NOT NULL")
	}
	if !strings.Contains(s, "assessment_digest ~ '^[0-9a-f]{64}$'") {
		t.Error("assessment_digest must enforce lowercase-hex-64 CHECK (~ '^[0-9a-f]{64}$')")
	}
	// 不得再有 DEFAULT '' 兜底（否则空摘要可绕过写入契约）。
	if strings.Contains(lower, "assessment_digest") && strings.Contains(lower, "default ''") {
		// 精确检查同一行。
		for _, line := range ddlLines {
			ll := strings.ToLower(line)
			if strings.Contains(ll, "assessment_digest") && strings.Contains(ll, "default ''") {
				t.Error("assessment_digest must NOT have DEFAULT '' fallback")
			}
		}
	}
}
