package admin

import (
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

type PromptAuditHandler struct {
	service *service.PromptAuditService
}

func NewPromptAuditHandler(svc *service.PromptAuditService) *PromptAuditHandler {
	return &PromptAuditHandler{service: svc}
}

// Service 供网关捕获中间件取用底层服务（避免给 GatewayHandler 增加构造参数）。
func (h *PromptAuditHandler) Service() *service.PromptAuditService {
	if h == nil {
		return nil
	}
	return h.service
}

type promptAuditConfigRequest struct {
	Enabled       *bool `json:"enabled"`
	RetentionDays *int  `json:"retention_days"`
}

func (h *PromptAuditHandler) GetConfig(c *gin.Context) {
	cfg, err := h.service.GetConfig(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, cfg)
}

func (h *PromptAuditHandler) UpdateConfig(c *gin.Context) {
	var req promptAuditConfigRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	// 以当前配置为基线,仅覆盖传入字段。
	cfg, err := h.service.GetConfig(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if req.Enabled != nil {
		cfg.Enabled = *req.Enabled
	}
	if req.RetentionDays != nil {
		cfg.RetentionDays = *req.RetentionDays
	}
	updated, err := h.service.UpdateConfig(c.Request.Context(), cfg)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, updated)
}

func (h *PromptAuditHandler) GetStatus(c *gin.Context) {
	response.Success(c, h.service.Status(c.Request.Context()))
}

func (h *PromptAuditHandler) ListEvents(c *gin.Context) {
	page, pageSize := response.ParsePagination(c)
	filter := service.PromptAuditEventFilter{
		Search: strings.TrimSpace(c.Query("search")),
		Pagination: pagination.PaginationParams{
			Page:      page,
			PageSize:  pageSize,
			SortOrder: pagination.SortOrderDesc,
		},
	}
	if raw := strings.TrimSpace(c.Query("group_id")); raw != "" {
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || v <= 0 {
			response.BadRequest(c, "Invalid group_id")
			return
		}
		filter.GroupID = &v
	}
	if raw := strings.TrimSpace(c.Query("api_key_id")); raw != "" {
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || v <= 0 {
			response.BadRequest(c, "Invalid api_key_id")
			return
		}
		filter.APIKeyID = &v
	}
	if raw := strings.TrimSpace(c.Query("user_id")); raw != "" {
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || v <= 0 {
			response.BadRequest(c, "Invalid user_id")
			return
		}
		filter.UserID = &v
	}
	if raw := strings.TrimSpace(c.Query("from")); raw != "" {
		t, _, err := parsePromptAuditDate(raw)
		if err != nil {
			response.BadRequest(c, "Invalid from")
			return
		}
		filter.From = &t
	}
	if raw := strings.TrimSpace(c.Query("to")); raw != "" {
		t, dateOnly, err := parsePromptAuditDate(raw)
		if err != nil {
			response.BadRequest(c, "Invalid to")
			return
		}
		if dateOnly {
			t = t.Add(24*time.Hour - time.Nanosecond)
		}
		filter.To = &t
	}
	items, pageResult, err := h.service.ListEvents(c.Request.Context(), filter)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Paginated(c, items, pageResult.Total, pageResult.Page, pageResult.PageSize)
}

func (h *PromptAuditHandler) GetEvent(c *gin.Context) {
	id, err := strconv.ParseInt(strings.TrimSpace(c.Param("id")), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid id")
		return
	}
	event, err := h.service.GetEvent(c.Request.Context(), id)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if event == nil {
		response.NotFound(c, "Prompt audit event not found")
		return
	}
	response.Success(c, event)
}

func (h *PromptAuditHandler) DeleteEvent(c *gin.Context) {
	id, err := strconv.ParseInt(strings.TrimSpace(c.Param("id")), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid id")
		return
	}
	if err := h.service.DeleteEvent(c.Request.Context(), id); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"deleted": true})
}

func (h *PromptAuditHandler) DeleteAll(c *gin.Context) {
	n, err := h.service.DeleteAll(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"deleted": n})
}

func parsePromptAuditDate(raw string) (time.Time, bool, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false, nil
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t, false, nil
	}
	t, err := time.Parse("2006-01-02", raw)
	return t, err == nil, err
}
