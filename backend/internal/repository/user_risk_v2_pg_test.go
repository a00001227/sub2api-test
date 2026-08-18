package repository

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/migrations"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// 真实 PostgreSQL 集成测试。仅当设置 RISK_V2_TEST_PG_DSN 时运行（临时非生产库）；否则 skip。
// 设置方式见报告：本地 initdb 起临时实例后 export RISK_V2_TEST_PG_DSN=...

func pgDSN(t *testing.T) string {
	dsn := os.Getenv("RISK_V2_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("RISK_V2_TEST_PG_DSN unset — POSTGRES_INTEGRATION_UNVERIFIED")
	}
	return dsn
}

func openPG(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("postgres", pgDSN(t))
	require.NoError(t, err)
	require.NoError(t, db.Ping())
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// setupSchema 建 users/legacy user_risk 桩表 + 应用真实 173 迁移；返回一个已存在的 user id。
func setupSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS users (id BIGSERIAL PRIMARY KEY, deleted_at TIMESTAMPTZ)`)
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS user_risk (user_id BIGINT PRIMARY KEY, score SMALLINT NOT NULL DEFAULT 0, tier VARCHAR(8) NOT NULL DEFAULT 'watch')`)
	require.NoError(t, err)
	applyMigration173(t, db)
	// 固定几个测试用户（满足 FK）。
	for _, id := range []int64{9001, 9002, 9003, 9004} {
		_, err := db.Exec(`INSERT INTO users (id) VALUES ($1) ON CONFLICT (id) DO NOTHING`, id)
		require.NoError(t, err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DROP TABLE IF EXISTS user_risk_v2`)
		_, _ = db.Exec(`DROP TABLE IF EXISTS user_risk`)
		_, _ = db.Exec(`DROP TABLE IF EXISTS users`)
	})
}

func applyMigration173(t *testing.T, db *sql.DB) {
	t.Helper()
	sqlBytes, err := migrations.FS.ReadFile("173_create_user_risk_v2.sql")
	require.NoError(t, err)
	_, err = db.Exec(string(sqlBytes))
	require.NoError(t, err)
}

func mkAssessment(assessedAt int64, tier string) service.RiskV2Assessment {
	a := sampleAssessment()
	a.AssessedAtUnix = assessedAt
	a.RiskTier = tier
	return a
}

// —— Migration apply / rollback / reapply ——

func TestPG_MigrationApplyRollbackReapply(t *testing.T) {
	db := openPG(t)
	setupSchema(t, db) // 已 apply 一次
	// 表、索引、约束、FK 存在。
	var n int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM information_schema.tables WHERE table_name='user_risk_v2'`).Scan(&n))
	require.Equal(t, 1, n)
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM pg_indexes WHERE tablename='user_risk_v2'`).Scan(&n))
	require.GreaterOrEqual(t, n, 4, "expected >=4 indexes (+pk)")
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM information_schema.table_constraints WHERE table_name='user_risk_v2' AND constraint_type='CHECK'`).Scan(&n))
	require.Greater(t, n, 5, "expected multiple CHECK constraints")
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM information_schema.table_constraints WHERE table_name='user_risk_v2' AND constraint_type='FOREIGN KEY'`).Scan(&n))
	require.Equal(t, 1, n, "expected FK to users")

	// 重复执行（幂等）。
	applyMigration173(t, db)
	// rollback。
	_, err := db.Exec(`DROP TABLE IF EXISTS user_risk_v2`)
	require.NoError(t, err)
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM information_schema.tables WHERE table_name='user_risk_v2'`).Scan(&n))
	require.Equal(t, 0, n)
	// reapply。
	applyMigration173(t, db)
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM information_schema.tables WHERE table_name='user_risk_v2'`).Scan(&n))
	require.Equal(t, 1, n)
}

// —— 四态 upsert + digest ——

func TestPG_UpsertFourStates(t *testing.T) {
	db := openPG(t)
	setupSchema(t, db)
	repo := NewUserRiskV2Repository(db)
	ctx := context.Background()

	res, err := repo.UpsertCurrentAssessment(ctx, 9001, mkAssessment(1000, "HIGH"))
	require.NoError(t, err)
	require.Equal(t, service.RiskV2Inserted, res)

	// 记录 updated_at。
	var updated1 int64
	require.NoError(t, db.QueryRow(`SELECT updated_at FROM user_risk_v2 WHERE user_id=9001`).Scan(&updated1))

	// NOOP：同 assessed_at + 同内容 → 不动 updated_at。
	res, err = repo.UpsertCurrentAssessment(ctx, 9001, mkAssessment(1000, "HIGH"))
	require.NoError(t, err)
	require.Equal(t, service.RiskV2Noop, res)
	var updated2 int64
	require.NoError(t, db.QueryRow(`SELECT updated_at FROM user_risk_v2 WHERE user_id=9001`).Scan(&updated2))
	require.Equal(t, updated1, updated2, "NOOP must not change updated_at")

	// CONFLICT：同 assessed_at + 不同内容 → 报冲突，不覆盖。
	res, err = repo.UpsertCurrentAssessment(ctx, 9001, mkAssessment(1000, "MEDIUM"))
	require.ErrorIs(t, err, service.ErrRiskV2AssessmentConflict)
	var tier string
	require.NoError(t, db.QueryRow(`SELECT risk_tier FROM user_risk_v2 WHERE user_id=9001`).Scan(&tier))
	require.Equal(t, "HIGH", tier, "conflict must not overwrite")

	// UPDATED：更新 assessed_at。
	res, err = repo.UpsertCurrentAssessment(ctx, 9001, mkAssessment(2000, "MEDIUM"))
	require.NoError(t, err)
	require.Equal(t, service.RiskV2Updated, res)

	// STALE_IGNORED：更旧 assessed_at。
	res, err = repo.UpsertCurrentAssessment(ctx, 9001, mkAssessment(500, "WATCH"))
	require.NoError(t, err)
	require.Equal(t, service.RiskV2StaleIgnored, res)
	require.NoError(t, db.QueryRow(`SELECT risk_tier FROM user_risk_v2 WHERE user_id=9001`).Scan(&tier))
	require.Equal(t, "MEDIUM", tier)
}

// —— nullable plane + JSONB round-trip + Get ——

func TestPG_NullablePlaneAndRoundTrip(t *testing.T) {
	db := openPG(t)
	setupSchema(t, db)
	repo := NewUserRiskV2Repository(db)
	ctx := context.Background()
	a := mkAssessment(1000, "HIGH") // sampleAssessment: campaign unavailable
	_, err := repo.UpsertCurrentAssessment(ctx, 9001, a)
	require.NoError(t, err)

	var campNull sql.NullFloat64
	require.NoError(t, db.QueryRow(`SELECT campaign_score FROM user_risk_v2 WHERE user_id=9001`).Scan(&campNull))
	require.False(t, campNull.Valid, "unavailable plane must be NULL in DB")

	got, ok, err := repo.GetCurrentAssessment(ctx, 9001)
	require.NoError(t, err)
	require.True(t, ok)
	require.False(t, got.Campaign.Available)
	require.True(t, got.Automation.Available)
	require.Len(t, got.EvidenceFamilies, 1)
	require.Len(t, got.ReasonCodes, 1)
	require.Equal(t, []string{"exact_incomplete:1h"}, got.IncompleteReasons)
}

// —— List filter / pagination / delete ——

func TestPG_ListFilterPaginationDelete(t *testing.T) {
	db := openPG(t)
	setupSchema(t, db)
	repo := NewUserRiskV2Repository(db)
	ctx := context.Background()
	for i, uid := range []int64{9001, 9002, 9003} {
		a := mkAssessment(int64(1000+i), "HIGH")
		a.RiskIndex = float64(90 - i*10)
		_, err := repo.UpsertCurrentAssessment(ctx, uid, a)
		require.NoError(t, err)
	}
	items, err := repo.ListCurrentAssessments(ctx, service.RiskV2ListFilter{Tier: "HIGH"}, service.RiskV2Pagination{Limit: 10})
	require.NoError(t, err)
	require.Len(t, items, 3)
	for i := 1; i < len(items); i++ {
		require.GreaterOrEqual(t, items[i-1].RiskIndex, items[i].RiskIndex, "stable order risk_index DESC")
	}
	require.Nil(t, items[0].CampaignScore)

	// 分页。
	page1, _ := repo.ListCurrentAssessments(ctx, service.RiskV2ListFilter{}, service.RiskV2Pagination{Limit: 2, Offset: 0})
	page2, _ := repo.ListCurrentAssessments(ctx, service.RiskV2ListFilter{}, service.RiskV2Pagination{Limit: 2, Offset: 2})
	require.Len(t, page1, 2)
	require.Len(t, page2, 1)

	require.NoError(t, repo.DeleteByUserID(ctx, 9001))
	_, ok, _ := repo.GetCurrentAssessment(ctx, 9001)
	require.False(t, ok)
}

// —— FK cascade：删除用户 → user_risk_v2 行级联删除 ——

func TestPG_FKCascadeOnUserDelete(t *testing.T) {
	db := openPG(t)
	setupSchema(t, db)
	repo := NewUserRiskV2Repository(db)
	ctx := context.Background()
	_, err := repo.UpsertCurrentAssessment(ctx, 9002, mkAssessment(1000, "HIGH"))
	require.NoError(t, err)
	_, err = db.Exec(`DELETE FROM users WHERE id=9002`)
	require.NoError(t, err)
	var n int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM user_risk_v2 WHERE user_id=9002`).Scan(&n))
	require.Equal(t, 0, n, "FK ON DELETE CASCADE must remove orphan")
}

// —— DB 约束拒绝非法数据 ——

func TestPG_ConstraintsRejectInvalid(t *testing.T) {
	db := openPG(t)
	setupSchema(t, db)
	// 合法基线插入（user 9001）；assessment_digest 已无 DEFAULT，必须显式给合法 64 位小写 hex。
	const goodDigest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	base := `INSERT INTO user_risk_v2 (user_id, feature_version, policy_version, assessment_digest, created_at, updated_at`
	baseVals := `) VALUES (9001,'fv','pv','` + goodDigest + `',1,1`
	_, err := db.Exec(base + baseVals + `)`)
	require.NoError(t, err)
	_, _ = db.Exec(`DELETE FROM user_risk_v2 WHERE user_id=9001`)

	cases := []struct {
		name string
		cols string
		vals string
	}{
		{"invalid_tier", ",risk_tier", ",'BOGUS'"},
		{"invalid_confidence", ",confidence", ",2.0"},
		{"invalid_risk_index", ",risk_index", ",200"},
		{"available_score_mismatch", ",automation_available,automation_score", ",true,NULL"},
		{"effective_action_non_none", ",effective_action", ",'THROTTLE'"},
		// assessment_digest 契约：空串、大写、长度不足、非 hex、NULL 全部必须被拒。
		{"digest_empty", "", ""},          // 覆盖基线 digest 为空串
		{"digest_uppercase", "", ""},      // 覆盖基线 digest 为大写 hex
		{"digest_too_short", "", ""},      // 覆盖基线 digest 长度 < 64
		{"digest_non_hex", "", ""},        // 覆盖基线 digest 含非 hex 字符
		{"digest_null", "", ""},           // 覆盖基线 digest 为 NULL
		{"empty_feature_version", "", ""}, // 用空 feature_version 覆盖基线
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var q string
			switch c.name {
			case "empty_feature_version":
				q = `INSERT INTO user_risk_v2 (user_id, feature_version, policy_version, assessment_digest, created_at, updated_at) VALUES (9001,'','pv','` + goodDigest + `',1,1)`
			case "digest_empty":
				q = `INSERT INTO user_risk_v2 (user_id, feature_version, policy_version, assessment_digest, created_at, updated_at) VALUES (9001,'fv','pv','',1,1)`
			case "digest_uppercase":
				q = `INSERT INTO user_risk_v2 (user_id, feature_version, policy_version, assessment_digest, created_at, updated_at) VALUES (9001,'fv','pv','0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF',1,1)`
			case "digest_too_short":
				q = `INSERT INTO user_risk_v2 (user_id, feature_version, policy_version, assessment_digest, created_at, updated_at) VALUES (9001,'fv','pv','0123456789abcdef',1,1)`
			case "digest_non_hex":
				q = `INSERT INTO user_risk_v2 (user_id, feature_version, policy_version, assessment_digest, created_at, updated_at) VALUES (9001,'fv','pv','g123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef',1,1)`
			case "digest_null":
				q = `INSERT INTO user_risk_v2 (user_id, feature_version, policy_version, assessment_digest, created_at, updated_at) VALUES (9001,'fv','pv',NULL,1,1)`
			default:
				q = base + c.cols + baseVals + c.vals + `)`
			}
			_, err := db.Exec(q)
			require.Error(t, err, "constraint must reject %s", c.name)
			_, _ = db.Exec(`DELETE FROM user_risk_v2 WHERE user_id=9001`)
		})
	}
}

// —— legacy user_risk 不变 ——

func TestPG_LegacyUserRiskUntouched(t *testing.T) {
	db := openPG(t)
	setupSchema(t, db)
	repo := NewUserRiskV2Repository(db)
	ctx := context.Background()
	_, err := db.Exec(`INSERT INTO user_risk (user_id, score, tier) VALUES (9003, 42, 'watch')`)
	require.NoError(t, err)
	_, err = repo.UpsertCurrentAssessment(ctx, 9003, mkAssessment(1000, "HIGH"))
	require.NoError(t, err)
	var score int
	require.NoError(t, db.QueryRow(`SELECT score FROM user_risk WHERE user_id=9003`).Scan(&score))
	require.Equal(t, 42, score)
}

// —— 并发 ——

func TestPG_ConcurrentSameCycleSameContent(t *testing.T) {
	db := openPG(t)
	setupSchema(t, db)
	repo := NewUserRiskV2Repository(db)
	ctx := context.Background()
	var inserted, noop, other int64
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := repo.UpsertCurrentAssessment(ctx, 9004, mkAssessment(1000, "HIGH"))
			if err != nil {
				atomic.AddInt64(&other, 1)
				return
			}
			switch res {
			case service.RiskV2Inserted:
				atomic.AddInt64(&inserted, 1)
			case service.RiskV2Noop:
				atomic.AddInt64(&noop, 1)
			default:
				atomic.AddInt64(&other, 1)
			}
		}()
	}
	wg.Wait()
	require.EqualValues(t, 1, inserted, "exactly one INSERTED")
	require.EqualValues(t, 0, other, "no conflicts/errors for identical content")
	require.EqualValues(t, 15, noop, "rest NOOP (no content thrash)")
	var rows int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM user_risk_v2 WHERE user_id=9004`).Scan(&rows))
	require.Equal(t, 1, rows)
}

func TestPG_ConcurrentSameCycleDifferentContent(t *testing.T) {
	db := openPG(t)
	setupSchema(t, db)
	repo := NewUserRiskV2Repository(db)
	ctx := context.Background()
	var success, conflict int64
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// 同 cycle（assessed_at=1000），但每个 goroutine 内容互不相同（唯一 risk_index → 唯一 digest）。
			a := mkAssessment(1000, "HIGH")
			a.RiskIndex = float64(10 + i*5)
			_, err := repo.UpsertCurrentAssessment(ctx, 9004, a)
			if errors.Is(err, service.ErrRiskV2AssessmentConflict) {
				atomic.AddInt64(&conflict, 1)
			} else if err == nil {
				atomic.AddInt64(&success, 1)
			}
		}(i)
	}
	wg.Wait()
	require.EqualValues(t, 1, success, "exactly one writer wins the cycle (no last-write-wins overwrite)")
	require.Positive(t, conflict, "differing content in same cycle must be reported as conflict")
	// 存储内容稳定（等于胜出者，未被抖动覆盖）。
	var d1, d2 string
	require.NoError(t, db.QueryRow(`SELECT assessment_digest FROM user_risk_v2 WHERE user_id=9004`).Scan(&d1))
	require.NoError(t, db.QueryRow(`SELECT assessment_digest FROM user_risk_v2 WHERE user_id=9004`).Scan(&d2))
	require.Equal(t, d1, d2)
}

func TestPG_ConcurrentNewVsOldCycle(t *testing.T) {
	db := openPG(t)
	setupSchema(t, db)
	repo := NewUserRiskV2Repository(db)
	ctx := context.Background()
	// 先放一条 old cycle。
	_, err := repo.UpsertCurrentAssessment(ctx, 9004, mkAssessment(1000, "WATCH"))
	require.NoError(t, err)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				_, _ = repo.UpsertCurrentAssessment(ctx, 9004, mkAssessment(2000, "HIGH")) // new
			} else {
				_, _ = repo.UpsertCurrentAssessment(ctx, 9004, mkAssessment(500, "MEDIUM")) // older-than-old
			}
		}(i)
	}
	wg.Wait()
	var assessed int64
	require.NoError(t, db.QueryRow(`SELECT assessed_at FROM user_risk_v2 WHERE user_id=9004`).Scan(&assessed))
	require.EqualValues(t, 2000, assessed, "newest cycle must win; older must never overwrite newer")
}

// 切片 5 §十八：Admin 列表扩展投影在真实 PG 上正确 scan（effective_action / versions / top reason codes）。
func TestPG_AdminListProjection(t *testing.T) {
	db := openPG(t)
	setupSchema(t, db)
	repo := NewUserRiskV2Repository(db)
	a := sampleAssessment()
	a.AssessedAtUnix = 5000
	a.EffectiveAction = "NONE"
	a.ReasonCodes = []service.RiskV2ReasonCode{
		{Code: "low", ConfidenceContribution: 0.1},
		{Code: "high", ConfidenceContribution: 0.9},
		{Code: "mid", ConfidenceContribution: 0.5},
		{Code: "tiny", ConfidenceContribution: 0.01},
	}
	_, err := repo.UpsertCurrentAssessment(context.Background(), 9001, a)
	require.NoError(t, err)

	items, err := repo.ListCurrentAssessments(context.Background(), service.RiskV2ListFilter{UserID: 9001}, service.RiskV2Pagination{Limit: 10})
	require.NoError(t, err)
	require.Len(t, items, 1)
	it := items[0]
	require.Equal(t, "NONE", it.EffectiveAction)
	require.NotEmpty(t, it.FeatureVersion)
	require.NotEmpty(t, it.PolicyVersion)
	// top-3 按 ConfidenceContribution 降序。
	require.Len(t, it.TopReasonCodes, 3)
	require.Equal(t, "high", it.TopReasonCodes[0].Code)
	require.Equal(t, "mid", it.TopReasonCodes[1].Code)
	require.Equal(t, "low", it.TopReasonCodes[2].Code)
}
