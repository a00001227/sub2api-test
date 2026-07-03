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
	CreatedAt  time.Time
	UpdatedAt  time.Time

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
}

// IsValidFeedbackStatus reports whether s is an allowed status.
func IsValidFeedbackStatus(s string) bool {
	return s == FeedbackStatusPending || s == FeedbackStatusResolved
}
