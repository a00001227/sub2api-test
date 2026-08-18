package repository

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// Model Extraction Risk V2 Shadow — 切片 3：user_risk_v2 仓储（raw database/sql）。
// 只存每用户最新一条 Assessment；JSON 列经字段白名单转换（绝不直接序列化任意内部对象）。

const riskV2ListMaxLimit = 500

type userRiskV2Repository struct {
	db  *sql.DB
	now func() time.Time
}

// NewUserRiskV2Repository 构造 user_risk_v2 数据访问实例。
func NewUserRiskV2Repository(db *sql.DB) service.UserRiskV2Repository {
	return &userRiskV2Repository{db: db, now: time.Now}
}

// CheckSchemaReady 只读探针：`SELECT assessment_digest FROM user_risk_v2 WHERE false`——
// 同时验证表与该列存在，且不返回/写入任何行。错误只含表/列名，绝不含秘密。
func (r *userRiskV2Repository) CheckSchemaReady(ctx context.Context) error {
	if r == nil || r.db == nil {
		return errors.New("user_risk_v2: repository/db unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	rows, err := r.db.QueryContext(ctx, `SELECT assessment_digest FROM user_risk_v2 WHERE false`)
	if err != nil {
		return fmt.Errorf("user_risk_v2 schema not ready (table/assessment_digest column): %w", err)
	}
	_ = rows.Close()
	return nil
}

// —— 白名单持久化 DTO（只映射允许字段；绝不含 HMAC/prompt/id/secret）——

type pFeatureAvailability struct {
	Requests            bool `json:"requests"`
	Fingerprint         bool `json:"fingerprint"`
	Cache               bool `json:"cache"`
	ActiveMinutes       bool `json:"active_minutes"`
	NearDuplicate       bool `json:"near_duplicate"`
	ToolUse             bool `json:"tool_use"`
	StructuredOutput    bool `json:"structured_output"`
	ModelConcentration  bool `json:"model_concentration"`
	CandidateGeneration bool `json:"candidate_generation"`
	GradingOrRanking    bool `json:"grading_or_ranking"`
	RewriteOrVariation  bool `json:"rewrite_or_variation"`
	CodeGeneration      bool `json:"code_generation"`
}

type pEvidenceFamily struct {
	Family    string  `json:"family"`
	Group     string  `json:"group"`
	Available bool    `json:"available"`
	Strength  float64 `json:"strength"`
	MeetsHigh bool    `json:"meets_high"`
	Window    string  `json:"window"`
}

type pEvidenceGroup struct {
	Group    string  `json:"group"`
	MetHigh  bool    `json:"met_high"`
	Strength float64 `json:"strength"`
}

type pReasonCode struct {
	Code                   string  `json:"code"`
	Window                 string  `json:"window"`
	ObservedValue          float64 `json:"observed_value"`
	Threshold              float64 `json:"threshold"`
	EvidenceFamily         string  `json:"evidence_family"`
	EvidenceGroup          string  `json:"evidence_group"`
	ConfidenceContribution float64 `json:"confidence_contribution"`
}

func whitelistFeatureAvailability(fa service.RiskV2FeatureAvailability) pFeatureAvailability {
	return pFeatureAvailability{
		Requests: fa.Requests, Fingerprint: fa.Fingerprint, Cache: fa.Cache, ActiveMinutes: fa.ActiveMinutes,
		NearDuplicate: fa.NearDuplicate, ToolUse: fa.ToolUse, StructuredOutput: fa.StructuredOutput,
		ModelConcentration: fa.ModelConcentration, CandidateGeneration: fa.CandidateGeneration,
		GradingOrRanking: fa.GradingOrRanking, RewriteOrVariation: fa.RewriteOrVariation, CodeGeneration: fa.CodeGeneration,
	}
}

func marshalWhitelist(a service.RiskV2Assessment) (fa, ef, eg, rc []byte, err error) {
	faDTO := whitelistFeatureAvailability(a.FeatureAvailability)
	efDTO := make([]pEvidenceFamily, 0, len(a.EvidenceFamilies))
	for _, f := range a.EvidenceFamilies {
		efDTO = append(efDTO, pEvidenceFamily{Family: f.Family, Group: f.Group, Available: f.Available, Strength: f.Strength, MeetsHigh: f.MeetsHigh, Window: f.Window})
	}
	egDTO := make([]pEvidenceGroup, 0, len(a.EvidenceGroups))
	for _, g := range a.EvidenceGroups {
		egDTO = append(egDTO, pEvidenceGroup{Group: g.Group, MetHigh: g.MetHigh, Strength: g.Strength})
	}
	rcDTO := make([]pReasonCode, 0, len(a.ReasonCodes))
	for _, r := range a.ReasonCodes {
		rcDTO = append(rcDTO, pReasonCode{Code: r.Code, Window: r.Window, ObservedValue: r.ObservedValue, Threshold: r.Threshold, EvidenceFamily: r.EvidenceFamily, EvidenceGroup: r.EvidenceGroup, ConfidenceContribution: r.ConfidenceContribution})
	}
	if fa, err = json.Marshal(faDTO); err != nil {
		return
	}
	if ef, err = json.Marshal(efDTO); err != nil {
		return
	}
	if eg, err = json.Marshal(egDTO); err != nil {
		return
	}
	rc, err = json.Marshal(rcDTO)
	return
}

func planeNull(p service.RiskV2PlaneScore) sql.NullFloat64 {
	return sql.NullFloat64{Float64: p.Score, Valid: p.Available}
}

// riskV2AssessmentDigest 对白名单化+稳定排序后的持久化内容算 SHA-256（不含 assessed_at；
// 不含任何 prompt/response/hmac/simhash/api key/请求 id/secret）。用于同 cycle 同/异内容判定。
func riskV2AssessmentDigest(a service.RiskV2Assessment, faJSON, efJSON, egJSON, rcJSON, irJSON []byte, action string) string {
	h := sha256.New()
	writeBlob := func(b []byte) { h.Write(b); h.Write([]byte{0x1f}) }
	writeBlob(faJSON)
	writeBlob(efJSON)
	writeBlob(egJSON)
	writeBlob(rcJSON)
	writeBlob(irJSON)
	f := func(v float64) string { return strconv.FormatFloat(v, 'f', 6, 64) }
	plane := func(p service.RiskV2PlaneScore) string {
		if !p.Available {
			return "na"
		}
		return f(p.Score)
	}
	h.Write([]byte(strings.Join([]string{
		"ri=" + f(a.RiskIndex), "tier=" + a.RiskTier, "conf=" + f(a.Confidence),
		"ds=" + strconv.FormatBool(a.DataSufficient),
		"auto=" + plane(a.Automation), "harv=" + plane(a.Harvest), "camp=" + plane(a.Campaign), "expo=" + plane(a.Exposure),
		"deg=" + strconv.FormatBool(a.Degraded), "inc=" + strconv.FormatBool(a.Incomplete), "ha=" + strconv.FormatBool(a.HealthAvailable),
		"action=" + action, "fv=" + a.FeatureVersion, "pv=" + a.PolicyVersion, "fkv=" + a.FingerprintKeyVersion,
	}, "\x1e")))
	return hex.EncodeToString(h.Sum(nil))
}

// UpsertCurrentAssessment 四态 upsert（行锁 + digest）：
// INSERTED / UPDATED（assessed_at 更新）/ STALE_IGNORED（assessed_at 更旧）/
// NOOP（同 assessed_at 同 digest，不动 updated_at）/ ErrRiskV2AssessmentConflict（同 assessed_at 异 digest）。
func (r *userRiskV2Repository) UpsertCurrentAssessment(ctx context.Context, userID int64, a service.RiskV2Assessment) (service.RiskV2UpsertResult, error) {
	faJSON, efJSON, egJSON, rcJSON, err := marshalWhitelist(a)
	if err != nil {
		return service.RiskV2Noop, err
	}
	reasonsIncomplete := a.IncompleteReasons
	if reasonsIncomplete == nil {
		reasonsIncomplete = []string{}
	}
	irJSON, err := json.Marshal(reasonsIncomplete)
	if err != nil {
		return service.RiskV2Noop, err
	}
	action := a.EffectiveAction
	if action == "" {
		action = "NONE"
	}
	digest := riskV2AssessmentDigest(a, faJSON, efJSON, egJSON, rcJSON, irJSON, action)

	// 最多重试 3 次：并发首插时可能撞 PK 唯一冲突，退回读分支按四态处理。
	for attempt := 0; attempt < 3; attempt++ {
		res, err := r.upsertOnce(ctx, userID, a, faJSON, efJSON, egJSON, rcJSON, irJSON, action, digest)
		if isUniqueViolation(err) {
			continue // 行已被并发插入 → 重试走 UPDATE/NOOP/CONFLICT 分支
		}
		return res, err
	}
	return service.RiskV2Noop, errors.New("risk_v2: upsert retry exhausted")
}

func (r *userRiskV2Repository) upsertOnce(ctx context.Context, userID int64, a service.RiskV2Assessment,
	faJSON, efJSON, egJSON, rcJSON, irJSON []byte, action, digest string) (service.RiskV2UpsertResult, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return service.RiskV2Noop, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	var storedAssessed int64
	var storedDigest string
	row := tx.QueryRowContext(ctx, `SELECT assessed_at, assessment_digest FROM user_risk_v2 WHERE user_id = $1 FOR UPDATE`, userID)
	scanErr := row.Scan(&storedAssessed, &storedDigest)
	now := r.now().Unix()

	if errors.Is(scanErr, sql.ErrNoRows) {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO user_risk_v2 (
				user_id, risk_index, risk_tier, confidence, data_sufficient,
				automation_score, automation_available, harvest_score, harvest_available,
				campaign_score, campaign_available, exposure_score, exposure_available,
				feature_availability, evidence_families, evidence_groups, reason_codes, incomplete_reasons,
				degraded, incomplete, health_available, effective_action, assessment_digest,
				feature_version, policy_version, fingerprint_key_version,
				assessed_at, created_at, updated_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29)`,
			userID, a.RiskIndex, a.RiskTier, a.Confidence, a.DataSufficient,
			planeNull(a.Automation), a.Automation.Available, planeNull(a.Harvest), a.Harvest.Available,
			planeNull(a.Campaign), a.Campaign.Available, planeNull(a.Exposure), a.Exposure.Available,
			faJSON, efJSON, egJSON, rcJSON, irJSON,
			a.Degraded, a.Incomplete, a.HealthAvailable, action, digest,
			a.FeatureVersion, a.PolicyVersion, a.FingerprintKeyVersion,
			a.AssessedAtUnix, now, now,
		); err != nil {
			return service.RiskV2Noop, err
		}
		if err := tx.Commit(); err != nil {
			return service.RiskV2Noop, err
		}
		committed = true
		return service.RiskV2Inserted, nil
	}
	if scanErr != nil {
		return service.RiskV2Noop, scanErr
	}

	switch {
	case a.AssessedAtUnix > storedAssessed:
		if _, err := tx.ExecContext(ctx, `
			UPDATE user_risk_v2 SET
				risk_index=$2, risk_tier=$3, confidence=$4, data_sufficient=$5,
				automation_score=$6, automation_available=$7, harvest_score=$8, harvest_available=$9,
				campaign_score=$10, campaign_available=$11, exposure_score=$12, exposure_available=$13,
				feature_availability=$14, evidence_families=$15, evidence_groups=$16, reason_codes=$17, incomplete_reasons=$18,
				degraded=$19, incomplete=$20, health_available=$21, effective_action=$22, assessment_digest=$23,
				feature_version=$24, policy_version=$25, fingerprint_key_version=$26,
				assessed_at=$27, updated_at=$28
			WHERE user_id=$1`,
			userID, a.RiskIndex, a.RiskTier, a.Confidence, a.DataSufficient,
			planeNull(a.Automation), a.Automation.Available, planeNull(a.Harvest), a.Harvest.Available,
			planeNull(a.Campaign), a.Campaign.Available, planeNull(a.Exposure), a.Exposure.Available,
			faJSON, efJSON, egJSON, rcJSON, irJSON,
			a.Degraded, a.Incomplete, a.HealthAvailable, action, digest,
			a.FeatureVersion, a.PolicyVersion, a.FingerprintKeyVersion,
			a.AssessedAtUnix, now,
		); err != nil {
			return service.RiskV2Noop, err
		}
		if err := tx.Commit(); err != nil {
			return service.RiskV2Noop, err
		}
		committed = true
		return service.RiskV2Updated, nil
	case a.AssessedAtUnix < storedAssessed:
		_ = tx.Commit()
		committed = true
		return service.RiskV2StaleIgnored, nil
	default: // 同 assessed_at
		_ = tx.Commit()
		committed = true
		if digest == storedDigest {
			return service.RiskV2Noop, nil // 不写、不动 updated_at
		}
		return service.RiskV2Noop, service.ErrRiskV2AssessmentConflict
	}
}

// GetCurrentAssessment 读取并重建 Assessment。
func (r *userRiskV2Repository) GetCurrentAssessment(ctx context.Context, userID int64) (*service.RiskV2Assessment, bool, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT risk_index, risk_tier, confidence, data_sufficient,
		       automation_score, automation_available, harvest_score, harvest_available,
		       campaign_score, campaign_available, exposure_score, exposure_available,
		       feature_availability, evidence_families, evidence_groups, reason_codes,
		       degraded, incomplete, incomplete_reasons, health_available, effective_action,
		       feature_version, policy_version, fingerprint_key_version, assessed_at
		FROM user_risk_v2 WHERE user_id = $1
	`, userID)

	var a service.RiskV2Assessment
	var autoS, harvS, campS, expoS sql.NullFloat64
	var autoA, harvA, campA, expoA bool
	var faJSON, efJSON, egJSON, rcJSON, irJSON []byte
	var healthAvail bool
	if err := row.Scan(
		&a.RiskIndex, &a.RiskTier, &a.Confidence, &a.DataSufficient,
		&autoS, &autoA, &harvS, &harvA,
		&campS, &campA, &expoS, &expoA,
		&faJSON, &efJSON, &egJSON, &rcJSON,
		&a.Degraded, &a.Incomplete, &irJSON, &healthAvail, &a.EffectiveAction,
		&a.FeatureVersion, &a.PolicyVersion, &a.FingerprintKeyVersion, &a.AssessedAtUnix,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, err
	}
	a.Automation = service.RiskV2PlaneScore{Score: nullF(autoS), Available: autoA}
	a.Harvest = service.RiskV2PlaneScore{Score: nullF(harvS), Available: harvA}
	a.Campaign = service.RiskV2PlaneScore{Score: nullF(campS), Available: campA}
	a.Exposure = service.RiskV2PlaneScore{Score: nullF(expoS), Available: expoA}

	a.HealthAvailable = healthAvail
	var irArr []string
	_ = json.Unmarshal(irJSON, &irArr)
	a.IncompleteReasons = irArr
	var faDTO pFeatureAvailability
	_ = json.Unmarshal(faJSON, &faDTO)
	a.FeatureAvailability = service.RiskV2FeatureAvailability{
		Requests: faDTO.Requests, Fingerprint: faDTO.Fingerprint, Cache: faDTO.Cache, ActiveMinutes: faDTO.ActiveMinutes,
		NearDuplicate: faDTO.NearDuplicate, ToolUse: faDTO.ToolUse, StructuredOutput: faDTO.StructuredOutput,
		ModelConcentration: faDTO.ModelConcentration, CandidateGeneration: faDTO.CandidateGeneration,
		GradingOrRanking: faDTO.GradingOrRanking, RewriteOrVariation: faDTO.RewriteOrVariation, CodeGeneration: faDTO.CodeGeneration,
	}
	var efDTO []pEvidenceFamily
	_ = json.Unmarshal(efJSON, &efDTO)
	for _, f := range efDTO {
		a.EvidenceFamilies = append(a.EvidenceFamilies, service.RiskV2EvidenceFamily{Family: f.Family, Group: f.Group, Available: f.Available, Strength: f.Strength, MeetsHigh: f.MeetsHigh, Window: f.Window})
	}
	var egDTO []pEvidenceGroup
	_ = json.Unmarshal(egJSON, &egDTO)
	for _, g := range egDTO {
		a.EvidenceGroups = append(a.EvidenceGroups, service.RiskV2EvidenceGroup{Group: g.Group, MetHigh: g.MetHigh, Strength: g.Strength})
	}
	var rcDTO []pReasonCode
	_ = json.Unmarshal(rcJSON, &rcDTO)
	for _, rr := range rcDTO {
		a.ReasonCodes = append(a.ReasonCodes, service.RiskV2ReasonCode{Code: rr.Code, Window: rr.Window, ObservedValue: rr.ObservedValue, Threshold: rr.Threshold, EvidenceFamily: rr.EvidenceFamily, EvidenceGroup: rr.EvidenceGroup, ConfidenceContribution: rr.ConfidenceContribution})
	}
	return &a, true, nil
}

// buildRiskV2ListQuery 是纯查询构造（便于无 DB 单测过滤/分页/上限/稳定排序）。
func buildRiskV2ListQuery(filter service.RiskV2ListFilter, page service.RiskV2Pagination) (string, []any) {
	limit := page.Limit
	if limit <= 0 || limit > riskV2ListMaxLimit {
		limit = 100
	}
	offset := page.Offset
	if offset < 0 {
		offset = 0
	}
	var where []string
	var args []any
	add := func(cond string, val any) {
		args = append(args, val)
		where = append(where, cond+" $"+strconv.Itoa(len(args)))
	}
	if filter.Tier != "" {
		add("risk_tier =", filter.Tier)
	}
	if filter.MinRiskIndex != nil {
		add("risk_index >=", *filter.MinRiskIndex)
	}
	if filter.DataSufficient != nil {
		add("data_sufficient =", *filter.DataSufficient)
	}
	if filter.Degraded != nil {
		add("degraded =", *filter.Degraded)
	}
	if filter.UserID > 0 {
		add("user_id =", filter.UserID)
	}
	if filter.AssessedFromUnix > 0 {
		add("assessed_at >=", filter.AssessedFromUnix)
	}
	if filter.AssessedToUnix > 0 {
		add("assessed_at <=", filter.AssessedToUnix)
	}
	query := `
		SELECT user_id, risk_index, risk_tier, confidence, data_sufficient, degraded, incomplete,
		       automation_score, harvest_score, campaign_score, exposure_score,
		       fingerprint_key_version, assessed_at, updated_at
		FROM user_risk_v2`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	args = append(args, limit)
	limPos := "$" + strconv.Itoa(len(args))
	args = append(args, offset)
	offPos := "$" + strconv.Itoa(len(args))
	query += " ORDER BY risk_index DESC, assessed_at DESC, user_id ASC LIMIT " + limPos + " OFFSET " + offPos
	return query, args
}

// ListCurrentAssessments 有界列表（稳定排序 risk_index DESC, assessed_at DESC, user_id ASC）。
func (r *userRiskV2Repository) ListCurrentAssessments(ctx context.Context, filter service.RiskV2ListFilter, page service.RiskV2Pagination) ([]service.RiskV2AssessmentListItem, error) {
	query, args := buildRiskV2ListQuery(filter, page)
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []service.RiskV2AssessmentListItem
	for rows.Next() {
		var it service.RiskV2AssessmentListItem
		var autoS, harvS, campS, expoS sql.NullFloat64
		if err := rows.Scan(&it.UserID, &it.RiskIndex, &it.RiskTier, &it.Confidence, &it.DataSufficient, &it.Degraded, &it.Incomplete,
			&autoS, &harvS, &campS, &expoS, &it.FingerprintKeyVersion, &it.AssessedAtUnix, &it.UpdatedAtUnix); err != nil {
			return nil, err
		}
		it.AutomationScore = nullFPtr(autoS)
		it.HarvestScore = nullFPtr(harvS)
		it.CampaignScore = nullFPtr(campS)
		it.ExposureScore = nullFPtr(expoS)
		out = append(out, it)
	}
	return out, rows.Err()
}

func (r *userRiskV2Repository) DeleteByUserID(ctx context.Context, userID int64) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM user_risk_v2 WHERE user_id = $1`, userID)
	return err
}

func nullF(n sql.NullFloat64) float64 {
	if n.Valid {
		return n.Float64
	}
	return 0
}
func nullFPtr(n sql.NullFloat64) *float64 {
	if !n.Valid {
		return nil
	}
	v := n.Float64
	return &v
}
