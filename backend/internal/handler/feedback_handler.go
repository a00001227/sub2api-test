package handler

import (
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/handler/dto"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// FeedbackHandler handles user feedback submission and history.
type FeedbackHandler struct {
	feedbackService *service.FeedbackService
}

// NewFeedbackHandler creates a new FeedbackHandler.
func NewFeedbackHandler(feedbackService *service.FeedbackService) *FeedbackHandler {
	return &FeedbackHandler{feedbackService: feedbackService}
}

// CreateFeedbackRequest is the payload for submitting feedback.
type CreateFeedbackRequest struct {
	Type      string `json:"type" binding:"required,max=40"`
	Content   string `json:"content" binding:"required"`
	RequestID string `json:"request_id" binding:"omitempty,max=200"`
}

// Create handles submitting a new feedback.
// POST /api/v1/feedback
func (h *FeedbackHandler) Create(c *gin.Context) {
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		response.Unauthorized(c, "User not authenticated")
		return
	}

	var req CreateFeedbackRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}

	result, err := h.feedbackService.Create(c.Request.Context(), subject.UserID, service.CreateFeedbackInput{
		Type:      req.Type,
		Content:   req.Content,
		RequestID: req.RequestID,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	response.Created(c, dto.FeedbackFromService(result))
}

// List returns the current user's feedback history.
// GET /api/v1/feedback
func (h *FeedbackHandler) List(c *gin.Context) {
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		response.Unauthorized(c, "User not authenticated")
		return
	}

	items, err := h.feedbackService.GetUserHistory(c.Request.Context(), subject.UserID, 50)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	out := make([]dto.Feedback, 0, len(items))
	for i := range items {
		out = append(out, *dto.FeedbackFromService(&items[i]))
	}
	response.Success(c, out)
}

// GetByID returns a single feedback owned by the current user.
// GET /api/v1/feedback/:id
func (h *FeedbackHandler) GetByID(c *gin.Context) {
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		response.Unauthorized(c, "User not authenticated")
		return
	}

	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid feedback ID")
		return
	}

	item, err := h.feedbackService.GetByID(c.Request.Context(), id)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	// Ownership check
	if item.UserID != subject.UserID {
		response.NotFound(c, "feedback not found")
		return
	}

	response.Success(c, dto.FeedbackFromService(item))
}
