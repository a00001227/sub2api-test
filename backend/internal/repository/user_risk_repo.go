package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// Risk Phase 0（仅观测/影子模式）：user_risk 仓储（raw database/sql）。

type userRiskRepository struct {
	db *sql.DB
}

// NewUserRiskRepository 构造 user_risk 数据访问实例。
func NewUserRiskRepository(db *sql.DB) service.UserRiskRepository {
	return &userRiskRepository{db: db}
}

// Upsert 写入/更新评分。allowlisted / manual_tier 不在此覆盖（由专用接口维护）。
func (r *userRiskRepository) Upsert(ctx context.Context, rec *service.UserRiskRecord) error {
	if rec == nil {
		return nil
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO user_risk (user_id, score, tier, features, updated_at)
		VALUES ($1, $2, $3, $4, NOW())
		ON CONFLICT (user_id) DO UPDATE SET
			score = EXCLUDED.score,
			tier = EXCLUDED.tier,
			features = EXCLUDED.features,
			updated_at = EXCLUDED.updated_at
	`, rec.UserID, rec.Score, rec.Tier, nullBytes(rec.FeaturesRaw))
	return err
}

func (r *userRiskRepository) GetByUserID(ctx context.Context, userID int64) (*service.UserRiskRecord, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT user_id, score, tier, features, allowlisted, manual_tier, updated_at
		FROM user_risk WHERE user_id = $1
	`, userID)
	rec, err := scanUserRisk(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return rec, nil
}

func (r *userRiskRepository) List(ctx context.Context, tier string, limit int) ([]service.UserRiskListItem, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	query := `
		SELECT ur.user_id, ur.score, ur.tier, ur.features, ur.allowlisted, ur.manual_tier, ur.updated_at,
		       COALESCE(u.email, '')
		FROM user_risk ur
		LEFT JOIN users u ON u.id = ur.user_id AND u.deleted_at IS NULL
	`
	args := []any{}
	if tier != "" {
		query += " WHERE ur.tier = $1"
		args = append(args, tier)
		query += " ORDER BY ur.score DESC, ur.user_id ASC LIMIT $2"
		args = append(args, limit)
	} else {
		query += " ORDER BY ur.score DESC, ur.user_id ASC LIMIT $1"
		args = append(args, limit)
	}

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []service.UserRiskListItem
	for rows.Next() {
		var item service.UserRiskListItem
		var features []byte
		var manualTier sql.NullString
		if err := rows.Scan(
			&item.UserID, &item.Score, &item.Tier, &features,
			&item.Allowlisted, &manualTier, &item.UpdatedAt, &item.Email,
		); err != nil {
			return nil, err
		}
		item.FeaturesRaw = features
		if manualTier.Valid {
			mt := manualTier.String
			item.ManualTier = &mt
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (r *userRiskRepository) SetAllowlist(ctx context.Context, userID int64, on bool) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO user_risk (user_id, allowlisted, updated_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (user_id) DO UPDATE SET allowlisted = EXCLUDED.allowlisted, updated_at = NOW()
	`, userID, on)
	return err
}

func (r *userRiskRepository) SetManualTier(ctx context.Context, userID int64, tier *string) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO user_risk (user_id, manual_tier, updated_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (user_id) DO UPDATE SET manual_tier = EXCLUDED.manual_tier, updated_at = NOW()
	`, userID, nullStringPtr(tier))
	return err
}

// AggregateUsage 计算某用户 usage_logs 近 window 的聚合特征（评分 worker fallback / 观测）。
// 仅读取 Phase 0 新增的观测列，绝不涉及任何 prompt 原文。
func (r *userRiskRepository) AggregateUsage(ctx context.Context, userID int64, window time.Duration) (service.UserRiskUsageAggregate, error) {
	var agg service.UserRiskUsageAggregate
	since := time.Now().Add(-window)

	row := r.db.QueryRowContext(ctx, `
		SELECT
			COUNT(*)                                                     AS requests,
			COALESCE(SUM(output_tokens), 0)                             AS out_tokens,
			COALESCE(SUM(input_tokens + cache_read_tokens + cache_creation_tokens), 0) AS in_tokens,
			COUNT(DISTINCT prompt_simhash) FILTER (WHERE prompt_simhash IS NOT NULL) AS distinct_sim,
			COUNT(prompt_simhash) FILTER (WHERE prompt_simhash IS NOT NULL)          AS total_sim,
			COUNT(*) FILTER (WHERE message_count IS NOT NULL AND message_count <= 1) AS single_turn,
			COUNT(*) FILTER (WHERE message_count IS NOT NULL)                        AS total_turns,
			COUNT(*) FILTER (WHERE temperature = 0)                                  AS zero_temp,
			COUNT(*) FILTER (WHERE temperature IS NOT NULL)                          AS total_temp,
			COALESCE(SUM(actual_cost), 0)                                            AS spend
		FROM usage_logs
		WHERE user_id = $1 AND created_at >= $2
	`, userID, since)

	var zeroTemp, totalTemp int
	var spend float64
	if err := row.Scan(
		&agg.Requests24h, &agg.OutputTokens24h, &agg.InputTokens24h,
		&agg.DistinctSimhash, &agg.TotalSimhash,
		&agg.SingleTurn, &agg.TotalTurns,
		&zeroTemp, &totalTemp, &spend,
	); err != nil {
		return agg, err
	}
	// actual_cost 以 USDC 计价（float），换算为 micros（1 USDC = 1e6 micros）。观测用，非账本。
	agg.SpendMicros = int64(spend * 1_000_000)
	if totalTemp > 0 {
		agg.ZeroTempShare = float64(zeroTemp) / float64(totalTemp)
	}

	// 模型直方图（头部模型 + 种类数）。
	modelRows, err := r.db.QueryContext(ctx, `
		SELECT model, COUNT(*) AS cnt
		FROM usage_logs
		WHERE user_id = $1 AND created_at >= $2 AND model <> ''
		GROUP BY model
		ORDER BY cnt DESC
	`, userID, since)
	if err != nil {
		return agg, err
	}
	defer modelRows.Close()
	first := true
	for modelRows.Next() {
		var model string
		var cnt int
		if err := modelRows.Scan(&model, &cnt); err != nil {
			return agg, err
		}
		agg.ModelVariety++
		if first {
			agg.TopModel = model
			agg.TopModelCount = cnt
			first = false
		}
	}
	if err := modelRows.Err(); err != nil {
		return agg, err
	}

	// max_tokens 固定占比：最常见 max_tokens 取值的占比。
	pinRow := r.db.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(cnt), 0), COALESCE(SUM(cnt), 0)
		FROM (
			SELECT COUNT(*) AS cnt
			FROM usage_logs
			WHERE user_id = $1 AND created_at >= $2 AND max_tokens IS NOT NULL
			GROUP BY max_tokens
		) t
	`, userID, since)
	var topPin, totalPin int
	if err := pinRow.Scan(&topPin, &totalPin); err != nil {
		return agg, err
	}
	if totalPin > 0 {
		agg.MaxTokenPinShare = float64(topPin) / float64(totalPin)
	}

	return agg, nil
}

func scanUserRisk(row interface{ Scan(dest ...any) error }) (*service.UserRiskRecord, error) {
	rec := &service.UserRiskRecord{}
	var features []byte
	var manualTier sql.NullString
	if err := row.Scan(
		&rec.UserID, &rec.Score, &rec.Tier, &features,
		&rec.Allowlisted, &manualTier, &rec.UpdatedAt,
	); err != nil {
		return nil, err
	}
	rec.FeaturesRaw = features
	if manualTier.Valid {
		mt := manualTier.String
		rec.ManualTier = &mt
	}
	return rec, nil
}

func nullBytes(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

func nullStringPtr(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}
