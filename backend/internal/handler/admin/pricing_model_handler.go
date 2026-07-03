package admin

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// PricingModelHandler handles admin pricing model management.
type PricingModelHandler struct {
	svc *service.PricingDisplayService
}

// NewPricingModelHandler creates a PricingModelHandler.
func NewPricingModelHandler(svc *service.PricingDisplayService) *PricingModelHandler {
	return &PricingModelHandler{svc: svc}
}

// ---- Request types ----

type createPricingModelRequest struct {
	Model     string           `json:"model"      binding:"required,max=200"`
	ModelType service.ModelType `json:"model_type" binding:"required,oneof=text image"`
	UserType  service.UserType  `json:"user_type"  binding:"required,oneof=end_user channel_user"`
	Enabled   *bool            `json:"enabled"`

	// Text fields
	InputPrice          *float64 `json:"input_price"`
	OutputPrice         *float64 `json:"output_price"`
	CacheReadPrice      *float64 `json:"cache_read_price"`
	CacheWritePrice     *float64 `json:"cache_write_price"`
	OfficialInputPrice  *float64 `json:"official_input_price"`
	OfficialOutputPrice *float64 `json:"official_output_price"`

	// Image field: map of resolution -> price
	ImageResolutions map[string]float64 `json:"image_resolutions"`

	// Image-specific saving percent (optional override)
	SavingPercent *float64 `json:"saving_percent"`
}

type updatePricingModelRequest struct {
	Model     string            `json:"model"      binding:"omitempty,max=200"`
	ModelType service.ModelType `json:"model_type" binding:"omitempty,oneof=text image"`
	UserType  service.UserType  `json:"user_type"  binding:"omitempty,oneof=end_user channel_user"`
	Enabled   *bool             `json:"enabled"`

	// Text fields
	InputPrice          *float64 `json:"input_price"`
	OutputPrice         *float64 `json:"output_price"`
	CacheReadPrice      *float64 `json:"cache_read_price"`
	CacheWritePrice     *float64 `json:"cache_write_price"`
	OfficialInputPrice  *float64 `json:"official_input_price"`
	OfficialOutputPrice *float64 `json:"official_output_price"`

	// Image field
	ImageResolutions map[string]float64 `json:"image_resolutions"`

	// Image-specific saving percent override
	SavingPercent *float64 `json:"saving_percent"`
}

// List returns all pricing models.
// GET /api/v1/admin/pricing/models
func (h *PricingModelHandler) List(c *gin.Context) {
	dtos, err := h.svc.ListPricingModels(c.Request.Context())
	if err != nil {
		response.InternalError(c, "failed to list pricing models")
		return
	}
	response.Success(c, dtos)
}

// Create adds a new pricing model.
// POST /api/v1/admin/pricing/models
func (h *PricingModelHandler) Create(c *gin.Context) {
	var req createPricingModelRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, http.StatusBadRequest, err.Error())
		return
	}

	rec := &service.PricingModelRecord{
		Model:               req.Model,
		ModelType:           req.ModelType,
		UserType:            req.UserType,
		Enabled:             true,
		InputPrice:          req.InputPrice,
		OutputPrice:         req.OutputPrice,
		CacheReadPrice:      req.CacheReadPrice,
		CacheWritePrice:     req.CacheWritePrice,
		OfficialInputPrice:  req.OfficialInputPrice,
		OfficialOutputPrice: req.OfficialOutputPrice,
	}
	if req.Enabled != nil {
		rec.Enabled = *req.Enabled
	}

	if req.ModelType == service.ModelTypeImage && len(req.ImageResolutions) > 0 {
		j, err := json.Marshal(req.ImageResolutions)
		if err != nil {
			response.Error(c, http.StatusBadRequest, "invalid image_resolutions")
			return
		}
		s := string(j)
		rec.ImagePricingJSON = &s
	}

	if req.SavingPercent != nil && req.ModelType == service.ModelTypeImage {
		rec.SavingPercent = *req.SavingPercent
	}

	result, err := h.svc.CreatePricingModel(c.Request.Context(), rec)
	if err != nil {
		if errors.Is(err, service.ErrPricingModelExists) {
			response.Error(c, http.StatusConflict, "pricing model already exists for that model+user_type")
			return
		}
		response.InternalError(c, "failed to create pricing model")
		return
	}
	response.Created(c, service.ToAdminDTO(result))
}

// GetByID returns a single pricing model.
// GET /api/v1/admin/pricing/models/:id
func (h *PricingModelHandler) GetByID(c *gin.Context) {
	id, err := parseID(c)
	if err != nil {
		response.Error(c, http.StatusBadRequest, "invalid id")
		return
	}
	rec, err := h.svc.GetPricingModel(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, service.ErrPricingModelNotFound) {
			response.Error(c, http.StatusNotFound, "pricing model not found")
			return
		}
		response.InternalError(c, "failed to get pricing model")
		return
	}
	response.Success(c, service.ToAdminDTO(rec))
}

// Update modifies an existing pricing model.
// PUT /api/v1/admin/pricing/models/:id
func (h *PricingModelHandler) Update(c *gin.Context) {
	id, err := parseID(c)
	if err != nil {
		response.Error(c, http.StatusBadRequest, "invalid id")
		return
	}

	existing, err := h.svc.GetPricingModel(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, service.ErrPricingModelNotFound) {
			response.Error(c, http.StatusNotFound, "pricing model not found")
			return
		}
		response.InternalError(c, "failed to get pricing model")
		return
	}

	var req updatePricingModelRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, http.StatusBadRequest, err.Error())
		return
	}

	// Apply patch semantics: only overwrite fields that are explicitly set in the request.
	if req.Model != "" {
		existing.Model = req.Model
	}
	prevModelType := existing.ModelType
	if req.ModelType != "" {
		existing.ModelType = req.ModelType
	}
	if req.UserType != "" {
		existing.UserType = req.UserType
	}
	if req.Enabled != nil {
		existing.Enabled = *req.Enabled
	}

	// Text price fields: only overwrite when the request body includes the key (non-nil pointer).
	if req.InputPrice != nil {
		existing.InputPrice = req.InputPrice
	}
	if req.OutputPrice != nil {
		existing.OutputPrice = req.OutputPrice
	}
	if req.CacheReadPrice != nil {
		existing.CacheReadPrice = req.CacheReadPrice
	}
	if req.CacheWritePrice != nil {
		existing.CacheWritePrice = req.CacheWritePrice
	}
	if req.OfficialInputPrice != nil {
		existing.OfficialInputPrice = req.OfficialInputPrice
	}
	if req.OfficialOutputPrice != nil {
		existing.OfficialOutputPrice = req.OfficialOutputPrice
	}

	// Image resolutions
	if req.ImageResolutions != nil {
		j, err := json.Marshal(req.ImageResolutions)
		if err != nil {
			response.Error(c, http.StatusBadRequest, "invalid image_resolutions")
			return
		}
		s := string(j)
		existing.ImagePricingJSON = &s
	} else if existing.ModelType == service.ModelTypeText {
		existing.ImagePricingJSON = nil
	}

	// If model_type switched from text→image, clear the old auto-computed saving_percent
	// so it doesn't carry over until the admin explicitly sets one.
	if prevModelType == service.ModelTypeText && existing.ModelType == service.ModelTypeImage {
		existing.SavingPercent = 0
	}

	// Saving percent override for image models (explicit set only)
	if req.SavingPercent != nil && existing.ModelType == service.ModelTypeImage {
		existing.SavingPercent = *req.SavingPercent
	}

	result, err := h.svc.UpdatePricingModel(c.Request.Context(), existing)
	if err != nil {
		if errors.Is(err, service.ErrPricingModelExists) {
			response.Error(c, http.StatusConflict, "model+user_type combination already exists")
			return
		}
		if errors.Is(err, service.ErrPricingModelNotFound) {
			response.Error(c, http.StatusNotFound, "pricing model not found")
			return
		}
		response.InternalError(c, "failed to update pricing model")
		return
	}
	response.Success(c, service.ToAdminDTO(result))
}

// Delete removes a pricing model.
// DELETE /api/v1/admin/pricing/models/:id
func (h *PricingModelHandler) Delete(c *gin.Context) {
	id, err := parseID(c)
	if err != nil {
		response.Error(c, http.StatusBadRequest, "invalid id")
		return
	}
	if err := h.svc.DeletePricingModel(c.Request.Context(), id); err != nil {
		if errors.Is(err, service.ErrPricingModelNotFound) {
			response.Error(c, http.StatusNotFound, "pricing model not found")
			return
		}
		response.InternalError(c, "failed to delete pricing model")
		return
	}
	response.Success(c, gin.H{"deleted": true})
}

func parseID(c *gin.Context) (int64, error) {
	return strconv.ParseInt(c.Param("id"), 10, 64)
}
