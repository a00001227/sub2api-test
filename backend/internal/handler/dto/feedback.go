package dto

import (
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// Feedback is the response DTO for a user feedback / support ticket.
type Feedback struct {
	ID         int64      `json:"id"`
	UserID     int64      `json:"user_id"`
	Type       string     `json:"type"`
	Content    string     `json:"content"`
	RequestID  *string    `json:"request_id,omitempty"`
	Status     string     `json:"status"`
	AdminReply *string    `json:"admin_reply,omitempty"`
	RepliedAt  *time.Time `json:"replied_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`

	// UserEmail is optionally populated for admin listing.
	UserEmail string `json:"user_email,omitempty"`
}

// FeedbackFromService maps a service feedback to its DTO.
func FeedbackFromService(f *service.Feedback) *Feedback {
	if f == nil {
		return nil
	}
	out := &Feedback{
		ID:         f.ID,
		UserID:     f.UserID,
		Type:       f.Type,
		Content:    f.Content,
		RequestID:  f.RequestID,
		Status:     f.Status,
		AdminReply: f.AdminReply,
		RepliedAt:  f.RepliedAt,
		CreatedAt:  f.CreatedAt,
		UpdatedAt:  f.UpdatedAt,
	}
	if f.User != nil {
		out.UserEmail = f.User.Email
	}
	return out
}
