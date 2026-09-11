package repository

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

type promptAuditRepository struct {
	db *sql.DB
}

func NewPromptAuditRepository(db *sql.DB) service.PromptAuditRepository {
	return &promptAuditRepository{db: db}
}

func (r *promptAuditRepository) CreateEvent(ctx context.Context, event *service.PromptAuditEvent) error {
	if event == nil {
		return nil
	}
	var userID, apiKeyID, groupID any
	if event.UserID != nil {
		userID = *event.UserID
	}
	if event.APIKeyID != nil {
		apiKeyID = *event.APIKeyID
	}
	if event.GroupID != nil {
		groupID = *event.GroupID
	}
	err := r.db.QueryRowContext(ctx, `
INSERT INTO prompt_audit_events (
    request_id, user_id, user_email, api_key_id, api_key_name, group_id, group_name,
    provider, endpoint, protocol, model, prompt_hash, prompt_length, message_count, full_prompt
) VALUES (
    $1, $2, $3, $4, $5, $6, $7,
    $8, $9, $10, $11, $12, $13, $14, $15
) RETURNING id, created_at`,
		event.RequestID, userID, event.UserEmail, apiKeyID, event.APIKeyName, groupID, event.GroupName,
		event.Provider, event.Endpoint, event.Protocol, event.Model, event.PromptHash, event.PromptLength, event.MessageCount, event.FullPrompt,
	).Scan(&event.ID, &event.CreatedAt)
	if err != nil {
		return fmt.Errorf("insert prompt audit event: %w", err)
	}
	return nil
}

func (r *promptAuditRepository) ListEvents(ctx context.Context, filter service.PromptAuditEventFilter) ([]service.PromptAuditEvent, *pagination.PaginationResult, error) {
	where, args := buildPromptAuditEventWhere(filter)
	whereSQL := "WHERE " + strings.Join(where, " AND ")

	// 结果过滤时 WHERE 会引用 m.*，COUNT 也需带上 lateral join；无结果过滤则保持快路径。
	countJoin := ""
	if promptAuditResultPredicate(filter.Result) != "" {
		countJoin = promptAuditModerationJoin
	}
	var total int64
	if err := r.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM prompt_audit_events e "+countJoin+whereSQL, args...).Scan(&total); err != nil {
		return nil, nil, fmt.Errorf("count prompt audit events: %w", err)
	}

	params := filter.Pagination
	if params.Page <= 0 {
		params.Page = 1
	}
	if params.PageSize <= 0 {
		params.PageSize = 20
	}
	if params.PageSize > 100 {
		params.PageSize = 100
	}
	queryArgs := append([]any{}, args...)
	queryArgs = append(queryArgs, params.Limit(), params.Offset())
	// 列表不返回 full_prompt（可能很大），仅返回长度/元数据；详情单独查。
	// 末尾 4 列为读时关联风控日志推导的结果字段（无关联记录时 action/category 为空、flagged=false、result=unaudited）。
	rows, err := r.db.QueryContext(ctx, `
SELECT
    e.id, e.request_id, e.user_id, e.user_email, e.api_key_id, e.api_key_name, e.group_id, e.group_name,
    e.provider, e.endpoint, e.protocol, e.model, e.prompt_hash, e.prompt_length, e.message_count,
    COALESCE(u.status, ''), e.created_at,
    COALESCE(m.action, ''), COALESCE(m.highest_category, ''), COALESCE(m.flagged, FALSE),
    `+promptAuditResultCase+`
FROM prompt_audit_events e
LEFT JOIN users u ON u.id = e.user_id`+promptAuditModerationJoin+whereSQL+`
ORDER BY e.created_at DESC, e.id DESC
LIMIT $`+fmt.Sprint(len(queryArgs)-1)+` OFFSET $`+fmt.Sprint(len(queryArgs)),
		queryArgs...,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("list prompt audit events: %w", err)
	}
	defer func() { _ = rows.Close() }()

	items := make([]service.PromptAuditEvent, 0)
	for rows.Next() {
		var item service.PromptAuditEvent
		var userID, apiKeyID, groupID sql.NullInt64
		if err := rows.Scan(
			&item.ID,
			&item.RequestID,
			&userID,
			&item.UserEmail,
			&apiKeyID,
			&item.APIKeyName,
			&groupID,
			&item.GroupName,
			&item.Provider,
			&item.Endpoint,
			&item.Protocol,
			&item.Model,
			&item.PromptHash,
			&item.PromptLength,
			&item.MessageCount,
			&item.UserStatus,
			&item.CreatedAt,
			&item.ModerationAction,
			&item.ModerationCategory,
			&item.ModerationFlagged,
			&item.Result,
		); err != nil {
			return nil, nil, fmt.Errorf("scan prompt audit event: %w", err)
		}
		if userID.Valid {
			v := userID.Int64
			item.UserID = &v
		}
		if apiKeyID.Valid {
			v := apiKeyID.Int64
			item.APIKeyID = &v
		}
		if groupID.Valid {
			v := groupID.Int64
			item.GroupID = &v
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("iterate prompt audit events: %w", err)
	}
	return items, paginationResultFromTotal(total, params), nil
}

// SummarizeResults 在给定过滤条件下按结果分桶计数（service 已清空 result，故这里不含结果过滤）。
// 用 FILTER 一次扫描出全部分桶，谓词与 promptAuditResultCase 一致。
func (r *promptAuditRepository) SummarizeResults(ctx context.Context, filter service.PromptAuditEventFilter) (service.PromptAuditResultSummary, error) {
	where, args := buildPromptAuditEventWhere(filter)
	whereSQL := "WHERE " + strings.Join(where, " AND ")

	var s service.PromptAuditResultSummary
	err := r.db.QueryRowContext(ctx, `
SELECT
    COUNT(*),
    COUNT(*) FILTER (WHERE `+promptAuditResultPredicate(service.PromptAuditResultAllow)+`),
    COUNT(*) FILTER (WHERE `+promptAuditResultPredicate(service.PromptAuditResultHit)+`),
    COUNT(*) FILTER (WHERE `+promptAuditResultPredicate(service.PromptAuditResultBlocked)+`),
    COUNT(*) FILTER (WHERE `+promptAuditResultPredicate(service.PromptAuditResultError)+`),
    COUNT(*) FILTER (WHERE `+promptAuditResultPredicate(service.PromptAuditResultUnaudited)+`)
FROM prompt_audit_events e`+promptAuditModerationJoin+whereSQL,
		args...,
	).Scan(&s.Total, &s.Allow, &s.Hit, &s.Blocked, &s.Error, &s.Unaudited)
	if err != nil {
		return service.PromptAuditResultSummary{}, fmt.Errorf("summarize prompt audit results: %w", err)
	}
	return s, nil
}

func (r *promptAuditRepository) GetEvent(ctx context.Context, id int64) (*service.PromptAuditEvent, error) {
	var item service.PromptAuditEvent
	var userID, apiKeyID, groupID sql.NullInt64
	err := r.db.QueryRowContext(ctx, `
SELECT
    e.id, e.request_id, e.user_id, e.user_email, e.api_key_id, e.api_key_name, e.group_id, e.group_name,
    e.provider, e.endpoint, e.protocol, e.model, e.prompt_hash, e.prompt_length, e.message_count,
    e.full_prompt, COALESCE(u.status, ''), e.created_at
FROM prompt_audit_events e
LEFT JOIN users u ON u.id = e.user_id
WHERE e.id = $1`, id).Scan(
		&item.ID,
		&item.RequestID,
		&userID,
		&item.UserEmail,
		&apiKeyID,
		&item.APIKeyName,
		&groupID,
		&item.GroupName,
		&item.Provider,
		&item.Endpoint,
		&item.Protocol,
		&item.Model,
		&item.PromptHash,
		&item.PromptLength,
		&item.MessageCount,
		&item.FullPrompt,
		&item.UserStatus,
		&item.CreatedAt,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("get prompt audit event: %w", err)
	}
	if userID.Valid {
		v := userID.Int64
		item.UserID = &v
	}
	if apiKeyID.Valid {
		v := apiKeyID.Int64
		item.APIKeyID = &v
	}
	if groupID.Valid {
		v := groupID.Int64
		item.GroupID = &v
	}
	return &item, nil
}

func (r *promptAuditRepository) DeleteEvent(ctx context.Context, id int64) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM prompt_audit_events WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete prompt audit event: %w", err)
	}
	return nil
}

func (r *promptAuditRepository) DeleteAll(ctx context.Context) (int64, error) {
	res, err := r.db.ExecContext(ctx, `DELETE FROM prompt_audit_events`)
	if err != nil {
		return 0, fmt.Errorf("delete all prompt audit events: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func (r *promptAuditRepository) CleanupExpired(ctx context.Context, before time.Time) (int64, error) {
	if r == nil || r.db == nil {
		return 0, nil
	}
	res, err := r.db.ExecContext(ctx, `DELETE FROM prompt_audit_events WHERE created_at < $1`, before)
	if err != nil {
		return 0, fmt.Errorf("delete expired prompt audit events: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// promptAuditModerationJoin 读时把每条提示词审计记录关联到「最近一条」同 request_id 的风控日志。
// LEFT JOIN 保证无关联记录（审核未开 / 抽样跳过 / request_id 为空）的行仍然返回（m.* 为 NULL）。
// 依赖 content_moderation_logs(request_id) 部分索引（见 migration 179）避免逐行 seq scan。
const promptAuditModerationJoin = ` LEFT JOIN LATERAL (
    SELECT cm.action, cm.flagged, cm.error, cm.highest_category
    FROM content_moderation_logs cm
    WHERE cm.request_id = e.request_id AND e.request_id <> ''
    ORDER BY cm.created_at DESC
    LIMIT 1
) m ON true `

// promptAuditResultCase 由关联到的风控 action/flagged/error 推导「结果」，与前端徽章一一对应。
// 优先级：无记录 → 异常 → 已拦截 → 命中 → 放行。必须与 promptAuditResultPredicate 保持一致。
const promptAuditResultCase = `CASE
    WHEN m.action IS NULL THEN 'unaudited'
    WHEN m.action = 'error' OR m.error <> '' THEN 'error'
    WHEN m.action IN ('block','hash_block','keyword_block','cyber_policy') THEN 'blocked'
    WHEN m.flagged THEN 'hit'
    ELSE 'allow'
END`

// promptAuditResultPredicate 返回某个结果值对应的 SQL 谓词（只用字面量、不占位参数，故不影响
// 既有 $N 参数编号）。逐条镜像 promptAuditResultCase 的优先级，供列表过滤与状态栏分桶复用。
func promptAuditResultPredicate(result string) string {
	const blocked = "m.action IN ('block','hash_block','keyword_block','cyber_policy')"
	const notError = "m.action <> 'error' AND m.error = ''"
	switch result {
	case service.PromptAuditResultUnaudited:
		return "m.action IS NULL"
	case service.PromptAuditResultError:
		return "m.action IS NOT NULL AND (m.action = 'error' OR m.error <> '')"
	case service.PromptAuditResultBlocked:
		return "m.action IS NOT NULL AND " + notError + " AND " + blocked
	case service.PromptAuditResultHit:
		return "m.action IS NOT NULL AND " + notError + " AND NOT (" + blocked + ") AND m.flagged IS TRUE"
	case service.PromptAuditResultAllow:
		return "m.action IS NOT NULL AND " + notError + " AND NOT (" + blocked + ") AND m.flagged IS NOT TRUE"
	}
	return ""
}

func buildPromptAuditEventWhere(filter service.PromptAuditEventFilter) ([]string, []any) {
	where := []string{"e.id IS NOT NULL"}
	args := make([]any, 0)
	add := func(expr string, value any) {
		args = append(args, value)
		where = append(where, fmt.Sprintf(expr, len(args)))
	}
	if filter.GroupID != nil {
		add("e.group_id = $%d", *filter.GroupID)
	}
	if filter.APIKeyID != nil {
		add("e.api_key_id = $%d", *filter.APIKeyID)
	}
	if filter.UserID != nil {
		add("e.user_id = $%d", *filter.UserID)
	}
	if search := strings.TrimSpace(filter.Search); search != "" {
		like := "%" + search + "%"
		args = append(args, like, like, like, like, like)
		idx := len(args) - 4
		where = append(where, fmt.Sprintf("(e.request_id ILIKE $%d OR e.user_email ILIKE $%d OR e.api_key_name ILIKE $%d OR e.model ILIKE $%d OR e.full_prompt ILIKE $%d)", idx, idx+1, idx+2, idx+3, idx+4))
	}
	if filter.From != nil && !filter.From.IsZero() {
		add("e.created_at >= $%d", *filter.From)
	}
	if filter.To != nil && !filter.To.IsZero() {
		add("e.created_at <= $%d", *filter.To)
	}
	// 结果过滤：谓词只用字面量、不占位参数，故不影响上面的 $N 编号；引用 m.* 需调用方带上
	// promptAuditModerationJoin。
	if pred := promptAuditResultPredicate(filter.Result); pred != "" {
		where = append(where, "("+pred+")")
	}
	return where, args
}
