package service

import (
	"context"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
)

// Feedback status values.
const (
	FeedbackStatusPending  = "pending"  // 未处理
	FeedbackStatusResolved = "resolved" // 已处理
)

// Feedback errors.
var (
	ErrFeedbackNotFound      = infraerrors.NotFound("FEEDBACK_NOT_FOUND", "feedback not found")
	ErrFeedbackInvalidStatus = infraerrors.BadRequest("FEEDBACK_STATUS_INVALID", "invalid feedback status")
)

// Feedback is the domain model for a user feedback / support ticket.
type Feedback struct {
	ID         int64
	UserID     int64
	Type       string
	Content    string
	RequestID  *string
	Status     string
	AdminReply *string
	RepliedAt  *time.Time
	// ReplyReadAt 用户查看管理员回复的时间;nil 或早于 RepliedAt = 有未读回复。
	ReplyReadAt *time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time

	// User is optionally populated for admin listing (submitter info).
	User *User
}

// FeedbackListFilters carries optional filters for admin listing.
type FeedbackListFilters struct {
	Status string
}

// FeedbackRepository is the persistence port for feedback.
type FeedbackRepository interface {
	Create(ctx context.Context, f *Feedback) error
	GetByID(ctx context.Context, id int64) (*Feedback, error)
	Update(ctx context.Context, f *Feedback) error
	List(ctx context.Context, params pagination.PaginationParams, filters FeedbackListFilters) ([]Feedback, *pagination.PaginationResult, error)
	ListByUser(ctx context.Context, userID int64, limit int) ([]Feedback, error)
	// CountUnreadReplies 统计该用户「有管理员回复且未读」的工单数(reply_read_at 为空或早于 replied_at)。
	CountUnreadReplies(ctx context.Context, userID int64) (int64, error)
	// MarkRepliesRead 把该用户所有未读回复的工单 reply_read_at 置为 at,返回受影响条数。
	MarkRepliesRead(ctx context.Context, userID int64, at time.Time) (int64, error)
}

// HasUnreadReply 是否有用户尚未查看的管理员回复。管理员再次回复会刷新 RepliedAt,自动重新变为未读。
func (f *Feedback) HasUnreadReply() bool {
	if f == nil || f.RepliedAt == nil {
		return false
	}
	return f.ReplyReadAt == nil || f.ReplyReadAt.Before(*f.RepliedAt)
}

// IsValidFeedbackStatus reports whether s is an allowed status.
func IsValidFeedbackStatus(s string) bool {
	return s == FeedbackStatusPending || s == FeedbackStatusResolved
}
