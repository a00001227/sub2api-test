package admin

import (
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/handler/dto"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// FeedbackHandler handles admin feedback management.
type FeedbackHandler struct {
	feedbackService *service.FeedbackService
}

// NewFeedbackHandler creates a new admin feedback handler.
func NewFeedbackHandler(feedbackService *service.FeedbackService) *FeedbackHandler {
	return &FeedbackHandler{feedbackService: feedbackService}
}

// List handles listing feedback with optional status filter.
// GET /api/v1/admin/feedbacks
func (h *FeedbackHandler) List(c *gin.Context) {
	page, pageSize := response.ParsePagination(c)
	status := strings.TrimSpace(c.Query("status"))
	sortBy := c.DefaultQuery("sort_by", "created_at")
	sortOrder := c.DefaultQuery("sort_order", "desc")

	params := pagination.PaginationParams{
		Page:      page,
		PageSize:  pageSize,
		SortBy:    sortBy,
		SortOrder: sortOrder,
	}

	items, paginationResult, err := h.feedbackService.List(
		c.Request.Context(),
		params,
		service.FeedbackListFilters{Status: status},
	)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	out := make([]dto.Feedback, 0, len(items))
	for i := range items {
		out = append(out, *dto.FeedbackFromService(&items[i]))
	}
	response.Paginated(c, out, paginationResult.Total, page, pageSize)
}

// GetByID handles getting a single feedback by ID.
// GET /api/v1/admin/feedbacks/:id
func (h *FeedbackHandler) GetByID(c *gin.Context) {
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
	response.Success(c, dto.FeedbackFromService(item))
}

// UpdateFeedbackStatusRequest is the payload for changing status.
type UpdateFeedbackStatusRequest struct {
	Status string `json:"status" binding:"required,oneof=pending resolved"`
}

// UpdateStatus handles changing a feedback's status.
// PUT /api/v1/admin/feedbacks/:id/status
func (h *FeedbackHandler) UpdateStatus(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid feedback ID")
		return
	}

	var req UpdateFeedbackStatusRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}

	item, err := h.feedbackService.UpdateStatus(c.Request.Context(), id, req.Status)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, dto.FeedbackFromService(item))
}

// ReplyFeedbackRequest is the payload for an admin reply.
type ReplyFeedbackRequest struct {
	Reply string `json:"reply" binding:"required"`
}

// Reply handles posting an admin reply (and marking the feedback resolved).
// PUT /api/v1/admin/feedbacks/:id/reply
func (h *FeedbackHandler) Reply(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid feedback ID")
		return
	}

	var req ReplyFeedbackRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}

	item, err := h.feedbackService.Reply(c.Request.Context(), id, req.Reply)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, dto.FeedbackFromService(item))
}
