package service

import (
	"context"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
)

// FeedbackService handles user feedback / support tickets.
type FeedbackService struct {
	feedbackRepo FeedbackRepository
	userRepo     UserRepository
}

// NewFeedbackService constructs a FeedbackService.
func NewFeedbackService(feedbackRepo FeedbackRepository, userRepo UserRepository) *FeedbackService {
	return &FeedbackService{feedbackRepo: feedbackRepo, userRepo: userRepo}
}

// CreateFeedbackInput is the payload for submitting feedback.
type CreateFeedbackInput struct {
	Type      string
	Content   string
	RequestID string
}

// Create submits a new feedback for the given user.
func (s *FeedbackService) Create(ctx context.Context, userID int64, input CreateFeedbackInput) (*Feedback, error) {
	feedbackType := strings.TrimSpace(input.Type)
	content := strings.TrimSpace(input.Content)
	if feedbackType == "" {
		feedbackType = "other"
	}

	f := &Feedback{
		UserID:  userID,
		Type:    feedbackType,
		Content: content,
		Status:  FeedbackStatusPending,
	}
	if rid := strings.TrimSpace(input.RequestID); rid != "" {
		f.RequestID = &rid
	}

	if err := s.feedbackRepo.Create(ctx, f); err != nil {
		return nil, err
	}
	return f, nil
}

// GetUserHistory returns the current user's own feedback, most recent first.
func (s *FeedbackService) GetUserHistory(ctx context.Context, userID int64, limit int) ([]Feedback, error) {
	if limit <= 0 {
		limit = 25
	}
	return s.feedbackRepo.ListByUser(ctx, userID, limit)
}

// GetByID returns a single feedback by id, with submitter user populated.
func (s *FeedbackService) GetByID(ctx context.Context, id int64) (*Feedback, error) {
	f, err := s.feedbackRepo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if s.userRepo != nil {
		if u, uerr := s.userRepo.GetByIDIncludeDeleted(ctx, f.UserID); uerr == nil {
			f.User = u
		}
	}
	return f, nil
}

// List returns paginated feedback (admin), optionally filtered by status.
// Submitter user info is populated for display.
func (s *FeedbackService) List(
	ctx context.Context,
	params pagination.PaginationParams,
	filters FeedbackListFilters,
) ([]Feedback, *pagination.PaginationResult, error) {
	items, result, err := s.feedbackRepo.List(ctx, params, filters)
	if err != nil {
		return nil, nil, err
	}
	s.populateUsers(ctx, items)
	return items, result, nil
}

// populateUsers fills the User field on each feedback by looking up submitters.
// Lookups are cached per page to avoid duplicate queries for the same user.
func (s *FeedbackService) populateUsers(ctx context.Context, items []Feedback) {
	if s.userRepo == nil || len(items) == 0 {
		return
	}
	cache := make(map[int64]*User)
	for i := range items {
		uid := items[i].UserID
		u, ok := cache[uid]
		if !ok {
			fetched, err := s.userRepo.GetByIDIncludeDeleted(ctx, uid)
			if err != nil {
				fetched = nil
			}
			u = fetched
			cache[uid] = u
		}
		items[i].User = u
	}
}

// UpdateStatus changes a feedback's status (admin).
func (s *FeedbackService) UpdateStatus(ctx context.Context, id int64, status string) (*Feedback, error) {
	if !IsValidFeedbackStatus(status) {
		return nil, ErrFeedbackInvalidStatus
	}
	f, err := s.feedbackRepo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	f.Status = status
	if err := s.feedbackRepo.Update(ctx, f); err != nil {
		return nil, err
	}
	return f, nil
}

// Reply sets the admin reply and marks the feedback as resolved.
func (s *FeedbackService) Reply(ctx context.Context, id int64, reply string) (*Feedback, error) {
	f, err := s.feedbackRepo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	trimmed := strings.TrimSpace(reply)
	now := time.Now()
	f.AdminReply = &trimmed
	f.RepliedAt = &now
	f.Status = FeedbackStatusResolved
	if err := s.feedbackRepo.Update(ctx, f); err != nil {
		return nil, err
	}
	return f, nil
}
