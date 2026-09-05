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

	var total int64
	if err := r.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM prompt_audit_events e "+whereSQL, args...).Scan(&total); err != nil {
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
	rows, err := r.db.QueryContext(ctx, `
SELECT
    e.id, e.request_id, e.user_id, e.user_email, e.api_key_id, e.api_key_name, e.group_id, e.group_name,
    e.provider, e.endpoint, e.protocol, e.model, e.prompt_hash, e.prompt_length, e.message_count,
    COALESCE(u.status, ''), e.created_at
FROM prompt_audit_events e
LEFT JOIN users u ON u.id = e.user_id `+whereSQL+`
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
	return where, args
}
