package repository

import (
	"context"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/feedback"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/service"

	entsql "entgo.io/ent/dialect/sql"
)

type feedbackRepository struct {
	client *dbent.Client
}

// NewFeedbackRepository constructs a feedback repository.
func NewFeedbackRepository(client *dbent.Client) service.FeedbackRepository {
	return &feedbackRepository{client: client}
}

func (r *feedbackRepository) Create(ctx context.Context, f *service.Feedback) error {
	client := clientFromContext(ctx, r.client)
	builder := client.Feedback.Create().
		SetUserID(f.UserID).
		SetType(f.Type).
		SetContent(f.Content).
		SetStatus(f.Status)

	if f.RequestID != nil {
		builder.SetRequestID(*f.RequestID)
	}

	created, err := builder.Save(ctx)
	if err != nil {
		return err
	}

	f.ID = created.ID
	f.CreatedAt = created.CreatedAt
	f.UpdatedAt = created.UpdatedAt
	return nil
}

func (r *feedbackRepository) GetByID(ctx context.Context, id int64) (*service.Feedback, error) {
	m, err := r.client.Feedback.Query().
		Where(feedback.IDEQ(id)).
		Only(ctx)
	if err != nil {
		return nil, translatePersistenceError(err, service.ErrFeedbackNotFound, nil)
	}
	return feedbackEntityToService(m), nil
}

func (r *feedbackRepository) Update(ctx context.Context, f *service.Feedback) error {
	client := clientFromContext(ctx, r.client)
	builder := client.Feedback.UpdateOneID(f.ID).
		SetType(f.Type).
		SetContent(f.Content).
		SetStatus(f.Status)

	if f.RequestID != nil {
		builder.SetRequestID(*f.RequestID)
	} else {
		builder.ClearRequestID()
	}
	if f.AdminReply != nil {
		builder.SetAdminReply(*f.AdminReply)
	} else {
		builder.ClearAdminReply()
	}
	if f.RepliedAt != nil {
		builder.SetRepliedAt(*f.RepliedAt)
	} else {
		builder.ClearRepliedAt()
	}
	if f.ReplyReadAt != nil {
		builder.SetReplyReadAt(*f.ReplyReadAt)
	} else {
		builder.ClearReplyReadAt()
	}

	updated, err := builder.Save(ctx)
	if err != nil {
		return translatePersistenceError(err, service.ErrFeedbackNotFound, nil)
	}
	f.UpdatedAt = updated.UpdatedAt
	return nil
}

func (r *feedbackRepository) List(
	ctx context.Context,
	params pagination.PaginationParams,
	filters service.FeedbackListFilters,
) ([]service.Feedback, *pagination.PaginationResult, error) {
	q := r.client.Feedback.Query()

	if filters.Status != "" {
		q = q.Where(feedback.StatusEQ(filters.Status))
	}

	total, err := q.Count(ctx)
	if err != nil {
		return nil, nil, err
	}

	itemsQuery := q.
		Offset(params.Offset()).
		Limit(params.Limit())
	for _, order := range feedbackListOrders(params) {
		itemsQuery = itemsQuery.Order(order)
	}

	items, err := itemsQuery.All(ctx)
	if err != nil {
		return nil, nil, err
	}

	return feedbackEntitiesToService(items), paginationResultFromTotal(int64(total), params), nil
}

func (r *feedbackRepository) ListByUser(ctx context.Context, userID int64, limit int) ([]service.Feedback, error) {
	items, err := r.client.Feedback.Query().
		Where(feedback.UserIDEQ(userID)).
		Order(dbent.Desc(feedback.FieldCreatedAt), dbent.Desc(feedback.FieldID)).
		Limit(limit).
		All(ctx)
	if err != nil {
		return nil, err
	}
	return feedbackEntitiesToService(items), nil
}

// unreadReplyPredicate 「有回复且未读」:replied_at 非空 且 (reply_read_at 为空 或 reply_read_at < replied_at)。
func unreadReplyPredicate(userID int64) []func(*entsql.Selector) {
	return []func(*entsql.Selector){
		feedback.UserIDEQ(userID),
		feedback.RepliedAtNotNil(),
		func(s *entsql.Selector) {
			s.Where(entsql.Or(
				entsql.IsNull(s.C(feedback.FieldReplyReadAt)),
				entsql.ColumnsLT(s.C(feedback.FieldReplyReadAt), s.C(feedback.FieldRepliedAt)),
			))
		},
	}
}

func (r *feedbackRepository) CountUnreadReplies(ctx context.Context, userID int64) (int64, error) {
	q := r.client.Feedback.Query()
	for _, p := range unreadReplyPredicate(userID) {
		q = q.Where(p)
	}
	n, err := q.Count(ctx)
	if err != nil {
		return 0, err
	}
	return int64(n), nil
}

func (r *feedbackRepository) MarkRepliesRead(ctx context.Context, userID int64, at time.Time) (int64, error) {
	client := clientFromContext(ctx, r.client)
	u := client.Feedback.Update()
	for _, p := range unreadReplyPredicate(userID) {
		u = u.Where(p)
	}
	n, err := u.SetReplyReadAt(at).Save(ctx)
	if err != nil {
		return 0, err
	}
	return int64(n), nil
}

func feedbackListOrders(params pagination.PaginationParams) []func(*entsql.Selector) {
	sortOrder := params.NormalizedSortOrder(pagination.SortOrderDesc)
	// Only created_at sorting is exposed; always tie-break on id for stability.
	if sortOrder == pagination.SortOrderAsc {
		return []func(*entsql.Selector){
			dbent.Asc(feedback.FieldCreatedAt),
			dbent.Asc(feedback.FieldID),
		}
	}
	return []func(*entsql.Selector){
		dbent.Desc(feedback.FieldCreatedAt),
		dbent.Desc(feedback.FieldID),
	}
}

func feedbackEntityToService(m *dbent.Feedback) *service.Feedback {
	if m == nil {
		return nil
	}
	return &service.Feedback{
		ID:          m.ID,
		UserID:      m.UserID,
		Type:        m.Type,
		Content:     m.Content,
		RequestID:   m.RequestID,
		Status:      m.Status,
		AdminReply:  m.AdminReply,
		RepliedAt:   m.RepliedAt,
		ReplyReadAt: m.ReplyReadAt,
		CreatedAt:   m.CreatedAt,
		UpdatedAt:   m.UpdatedAt,
	}
}

func feedbackEntitiesToService(models []*dbent.Feedback) []service.Feedback {
	out := make([]service.Feedback, 0, len(models))
	for i := range models {
		if s := feedbackEntityToService(models[i]); s != nil {
			out = append(out, *s)
		}
	}
	return out
}
