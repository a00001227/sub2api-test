package service

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"sync"
)

// PricingDisplayService computes the unified pricing display from pricing_models table.
// It is the single source of truth for all displayed prices; no frontend calculations allowed.
type PricingDisplayService struct {
	repo PricingModelRepository

	// publicCache caches the result of GetPublicPricingDisplay. Pricing data
	// changes very rarely (a handful of admin edits over months) but the public
	// endpoint is hit by every portal visitor, so we serve it from memory and
	// invalidate the cache whenever an admin mutates a pricing model.
	cacheMu     sync.RWMutex
	publicCache []PricingDisplayItem
	cacheValid  bool
}

// NewPricingDisplayService constructs PricingDisplayService.
func NewPricingDisplayService(repo PricingModelRepository) *PricingDisplayService {
	return &PricingDisplayService{repo: repo}
}

// invalidatePublicCache drops the cached public pricing display. Called after
// any admin create/update/delete so the next public read rebuilds from the DB.
func (s *PricingDisplayService) invalidatePublicCache() {
	s.cacheMu.Lock()
	s.publicCache = nil
	s.cacheValid = false
	s.cacheMu.Unlock()
}

// ---- Public API (portal-ui) DTOs ----

// PricingDisplayItem is the response DTO for the public pricing-display endpoint.
type PricingDisplayItem struct {
	Model     string       `json:"model"`
	ModelType ModelType    `json:"model_type"`
	UserType  UserType     `json:"user_type"`
	Pricing   PricingUnion `json:"pricing"`
}

// PricingUnion holds either text or image pricing (one will be nil).
type PricingUnion struct {
	Text  *TextPricingDTO  `json:"text,omitempty"`
	Image *ImagePricingDTO `json:"image,omitempty"`
}

// TextPricingDTO carries all text model pricing fields.
type TextPricingDTO struct {
	InputPrice          float64 `json:"input_price"`
	OutputPrice         float64 `json:"output_price"`
	CacheReadPrice      float64 `json:"cache_read_price"`
	CacheWritePrice     float64 `json:"cache_write_price"`
	OfficialInputPrice  float64 `json:"official_input_price"`
	OfficialOutputPrice float64 `json:"official_output_price"`
	SavingPercent       float64 `json:"saving_percent"`
}

// ImagePricingDTO carries dynamic resolution pricing for image models.
type ImagePricingDTO struct {
	// Resolutions maps resolution key (e.g. "1k", "2k") to price.
	Resolutions   map[string]float64 `json:"resolutions"`
	SavingPercent float64            `json:"saving_percent"`
}

// GetPublicPricingDisplay returns display-ready pricing for all enabled models.
// Results are cached in memory and invalidated on any admin mutation.
func (s *PricingDisplayService) GetPublicPricingDisplay(ctx context.Context) ([]PricingDisplayItem, error) {
	// Fast path: serve from cache.
	s.cacheMu.RLock()
	if s.cacheValid {
		cached := s.publicCache
		s.cacheMu.RUnlock()
		return cached, nil
	}
	s.cacheMu.RUnlock()

	records, err := s.repo.ListEnabled(ctx)
	if err != nil {
		return nil, fmt.Errorf("list enabled pricing models: %w", err)
	}

	items := make([]PricingDisplayItem, 0, len(records))
	for _, r := range records {
		item, err := s.buildDisplayItem(r)
		if err != nil {
			continue
		}
		items = append(items, item)
	}

	sort.Slice(items, func(i, j int) bool {
		if items[i].Model != items[j].Model {
			return items[i].Model < items[j].Model
		}
		return string(items[i].UserType) < string(items[j].UserType)
	})

	// Store in cache. The slice is treated as immutable after this point;
	// invalidation replaces it wholesale rather than mutating in place.
	s.cacheMu.Lock()
	s.publicCache = items
	s.cacheValid = true
	s.cacheMu.Unlock()

	return items, nil
}

func (s *PricingDisplayService) buildDisplayItem(r *PricingModelRecord) (PricingDisplayItem, error) {
	item := PricingDisplayItem{
		Model:     r.Model,
		ModelType: r.ModelType,
		UserType:  r.UserType,
	}

	switch r.ModelType {
	case ModelTypeText:
		item.Pricing.Text = buildTextDTO(r)
	case ModelTypeImage:
		dto, err := buildImageDTO(r)
		if err != nil {
			return item, err
		}
		item.Pricing.Image = dto
	}
	return item, nil
}

func buildTextDTO(r *PricingModelRecord) *TextPricingDTO {
	dto := &TextPricingDTO{}
	if r.InputPrice != nil {
		dto.InputPrice = *r.InputPrice
	}
	if r.OutputPrice != nil {
		dto.OutputPrice = *r.OutputPrice
	}
	if r.CacheReadPrice != nil {
		dto.CacheReadPrice = *r.CacheReadPrice
	}
	if r.CacheWritePrice != nil {
		dto.CacheWritePrice = *r.CacheWritePrice
	}
	if r.OfficialInputPrice != nil {
		dto.OfficialInputPrice = *r.OfficialInputPrice
	}
	if r.OfficialOutputPrice != nil {
		dto.OfficialOutputPrice = *r.OfficialOutputPrice
	}
	dto.SavingPercent = r.SavingPercent
	return dto
}

func buildImageDTO(r *PricingModelRecord) (*ImagePricingDTO, error) {
	resolutions := map[string]float64{}
	if r.ImagePricingJSON != nil && *r.ImagePricingJSON != "" {
		if err := json.Unmarshal([]byte(*r.ImagePricingJSON), &resolutions); err != nil {
			return nil, fmt.Errorf("parse image_pricing_json for model %s: %w", r.Model, err)
		}
	}
	return &ImagePricingDTO{
		Resolutions:   resolutions,
		SavingPercent: r.SavingPercent,
	}, nil
}

// ---- Admin DTOs ----

// PricingModelAdminDTO is the response DTO for admin endpoints.
type PricingModelAdminDTO struct {
	ID        int64     `json:"id"`
	Model     string    `json:"model"`
	ModelType ModelType `json:"model_type"`
	UserType  UserType  `json:"user_type"`
	Enabled   bool      `json:"enabled"`

	// Text fields
	InputPrice          *float64 `json:"input_price,omitempty"`
	OutputPrice         *float64 `json:"output_price,omitempty"`
	CacheReadPrice      *float64 `json:"cache_read_price,omitempty"`
	CacheWritePrice     *float64 `json:"cache_write_price,omitempty"`
	OfficialInputPrice  *float64 `json:"official_input_price,omitempty"`
	OfficialOutputPrice *float64 `json:"official_output_price,omitempty"`

	// Image field
	ImageResolutions map[string]float64 `json:"image_resolutions,omitempty"`

	// Computed
	SavingPercent float64 `json:"saving_percent"`
	UpdatedAt     string  `json:"updated_at"`
}

// ToAdminDTO converts a domain record to the admin response DTO.
func ToAdminDTO(r *PricingModelRecord) PricingModelAdminDTO {
	dto := PricingModelAdminDTO{
		ID:                  r.ID,
		Model:               r.Model,
		ModelType:           r.ModelType,
		UserType:            r.UserType,
		Enabled:             r.Enabled,
		InputPrice:          r.InputPrice,
		OutputPrice:         r.OutputPrice,
		CacheReadPrice:      r.CacheReadPrice,
		CacheWritePrice:     r.CacheWritePrice,
		OfficialInputPrice:  r.OfficialInputPrice,
		OfficialOutputPrice: r.OfficialOutputPrice,
		SavingPercent:       r.SavingPercent,
		UpdatedAt:           r.UpdatedAt.Format("2006-01-02T15:04:05Z"),
	}
	if r.ImagePricingJSON != nil && *r.ImagePricingJSON != "" {
		var m map[string]float64
		if err := json.Unmarshal([]byte(*r.ImagePricingJSON), &m); err == nil {
			dto.ImageResolutions = m
		}
	}
	return dto
}

// ---- Admin CRUD Service ----

// CreatePricingModel creates a new pricing model record and recomputes saving_percent.
func (s *PricingDisplayService) CreatePricingModel(ctx context.Context, r *PricingModelRecord) (*PricingModelRecord, error) {
	s.computeSavingPercent(r)
	if err := s.repo.Create(ctx, r); err != nil {
		return nil, err
	}
	s.invalidatePublicCache()
	return r, nil
}

// UpdatePricingModel updates an existing pricing model record.
func (s *PricingDisplayService) UpdatePricingModel(ctx context.Context, r *PricingModelRecord) (*PricingModelRecord, error) {
	s.computeSavingPercent(r)
	if err := s.repo.Update(ctx, r); err != nil {
		return nil, err
	}
	s.invalidatePublicCache()
	return r, nil
}

// DeletePricingModel removes a pricing model record.
func (s *PricingDisplayService) DeletePricingModel(ctx context.Context, id int64) error {
	if err := s.repo.Delete(ctx, id); err != nil {
		return err
	}
	s.invalidatePublicCache()
	return nil
}

// GetPricingModel retrieves a single record by ID.
func (s *PricingDisplayService) GetPricingModel(ctx context.Context, id int64) (*PricingModelRecord, error) {
	return s.repo.GetByID(ctx, id)
}

// ListPricingModels returns all pricing model records.
func (s *PricingDisplayService) ListPricingModels(ctx context.Context) ([]PricingModelAdminDTO, error) {
	records, err := s.repo.List(ctx)
	if err != nil {
		return nil, err
	}
	dtos := make([]PricingModelAdminDTO, 0, len(records))
	for _, r := range records {
		dtos = append(dtos, ToAdminDTO(r))
	}
	return dtos, nil
}

// computeSavingPercent calculates saving_percent based on model_type.
//
// Text model:
//
//	real_cost   = input_price + output_price (per-token sum as proxy)
//	official_io = official_input_price + official_output_price
//	saving      = (official_io - real_cost) / official_io
//
// Image model:
//
//	saving = average of per-resolution savings vs an implicit official price of 1.0
//	(since image models typically don't have a separate official price field, saving_percent
//	 is left at 0 unless the admin sets it explicitly; image savings are displayed per-resolution.)
func (s *PricingDisplayService) computeSavingPercent(r *PricingModelRecord) {
	switch r.ModelType {
	case ModelTypeText:
		input := derefFloat(r.InputPrice)
		output := derefFloat(r.OutputPrice)
		officialInput := derefFloat(r.OfficialInputPrice)
		officialOutput := derefFloat(r.OfficialOutputPrice)

		officialIO := officialInput + officialOutput
		realCost := input + output
		if officialIO > 0 {
			r.SavingPercent = math.Max(0, (officialIO-realCost)/officialIO)
		} else {
			r.SavingPercent = 0
		}
	case ModelTypeImage:
		// Image models: saving_percent field preserved as-is (admin can set via update).
		// No automatic computation without an official price field.
	}
}

func derefFloat(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}
