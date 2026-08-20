package admin

import (
	"errors"
	"net/http"
	"regexp"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// 疑似蒸馏取证：请求原文捕获 Admin API。管理员标记某 user/key → 捕获其后续 N 条请求原文（脱敏）→
// 查看/导出 → 手动清除。RBAC 复用 admin 路由组（未认证 401 / 非 admin 403 由上层中间件处理）。
// ⚠️ 这是「只存指纹、绝不存原文」的唯一例外，凭 admin 主动操作 + 服务条款取证声明。

const (
	evidenceErrBadRequest  = "EVIDENCE_BAD_REQUEST"
	evidenceErrUnavailable = "EVIDENCE_UNAVAILABLE"
	evidenceErrInternal    = "EVIDENCE_INTERNAL"
)

var evidenceTargetRe = regexp.MustCompile(`^[uk]:[0-9]+$`)

// EvidenceCaptureHandler 取证捕获 Admin Handler + 热路径捕获桥接。
type EvidenceCaptureHandler struct {
	svc *service.EvidenceCaptureService
}

// NewEvidenceCaptureHandler 构造。svc 为 nil 时路由不注册（见 registerEvidenceCaptureRoutes）。
func NewEvidenceCaptureHandler(svc *service.EvidenceCaptureService) *EvidenceCaptureHandler {
	return &EvidenceCaptureHandler{svc: svc}
}

type evidenceAPIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func evidenceErr(c *gin.Context, err error) {
	switch {
	case errors.Is(err, service.ErrEvidenceBadRequest):
		c.AbortWithStatusJSON(http.StatusBadRequest, evidenceAPIError{evidenceErrBadRequest, "invalid target/params"})
	case errors.Is(err, service.ErrEvidenceUnavailable):
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, evidenceAPIError{evidenceErrUnavailable, "evidence capture unavailable"})
	default:
		c.AbortWithStatusJSON(http.StatusInternalServerError, evidenceAPIError{evidenceErrInternal, "internal error"})
	}
}

func evidenceAdminID(c *gin.Context) int64 {
	if subj, ok := middleware2.GetAuthSubjectFromContext(c); ok {
		return subj.UserID
	}
	return 0
}

type startCaptureRequest struct {
	TargetType     string `json:"target_type"`     // "user" | "key"
	TargetID       int64  `json:"target_id"`
	StoreThreshold int    `json:"store_threshold"` // 同模板重复几次才存原文（<2 回落默认）
}

// StartCapture POST /admin/evidence/captures —— 标记某 user/key 开始重复模板取证。
func (h *EvidenceCaptureHandler) StartCapture(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	var req startCaptureRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		evidenceErr(c, service.ErrEvidenceBadRequest)
		return
	}
	f, err := h.svc.StartCapture(c.Request.Context(), service.EvidenceTargetType(req.TargetType), req.TargetID, req.StoreThreshold, evidenceAdminID(c))
	if err != nil {
		evidenceErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"capture": f})
}

// ListCaptures GET /admin/evidence/captures —— 当前活跃捕获名单 + 剩余计数。
func (h *EvidenceCaptureHandler) ListCaptures(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"captures": h.svc.ListActiveCaptures()})
}

// ListEvidence GET /admin/evidence/captures/:target —— 取该 target 已聚合的重复模板（含脱敏原文，供查看/导出）。
func (h *EvidenceCaptureHandler) ListEvidence(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	target := c.Param("target")
	if !evidenceTargetRe.MatchString(target) {
		evidenceErr(c, service.ErrEvidenceBadRequest)
		return
	}
	templates, err := h.svc.ListEvidenceTemplates(c.Request.Context(), target)
	if err != nil {
		evidenceErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"target": target, "count": len(templates), "templates": templates})
}

// PurgeEvidence DELETE /admin/evidence/captures/:target —— 清除证据 + 停止捕获。
func (h *EvidenceCaptureHandler) PurgeEvidence(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	target := c.Param("target")
	if !evidenceTargetRe.MatchString(target) {
		evidenceErr(c, service.ErrEvidenceBadRequest)
		return
	}
	if err := h.svc.PurgeEvidence(c.Request.Context(), target, evidenceAdminID(c)); err != nil {
		evidenceErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"purged": target})
}

// Capture 是热路径桥接（非 HTTP 端点）：由转发中间件回调，命中捕获名单则记一条请求原文。
// 默认无 flag 时一次原子读即返回（零开销），不解析 body、不碰 Redis。
func (h *EvidenceCaptureHandler) Capture(c *gin.Context, userID, apiKeyID int64, body []byte) {
	if h == nil || h.svc == nil || !h.svc.Active() {
		return
	}
	reqID, _ := c.Request.Context().Value(ctxkey.ClientRequestID).(string)
	meta := service.CaptureMeta{
		Model:     gjson.GetBytes(body, "model").String(),
		Endpoint:  c.Request.URL.Path,
		IP:        ip.GetClientIP(c),
		RequestID: reqID,
	}
	h.svc.CaptureIfFlagged(userID, apiKeyID, body, meta)
}
