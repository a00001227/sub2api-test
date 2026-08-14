package service

import (
	"context"
	"errors"
	"sort"
	"strings"
)

// pricing_catalog.go implements the "select models from the official catalog"
// admin flow for pricing_models. It mirrors the Provider Portal UX: list the
// official LiteLLM catalog (with an `added` flag per model), sync it from the
// remote source, and batch-create pricing_models rows (enabled=false / 待启用)
// from picked models with the official prices prefilled.
//
// Unlike the provider-internal ModelCatalog (which emits integer micros), the
// admin pricing feature works in USD FLOATS to match the pricing_models schema
// (per-token USD floats).

// catalogDefaultUserType is the user_type assigned to rows created from the
// catalog. It matches the default the manual Create path / dialog uses.
const catalogDefaultUserType = UserTypeEndUser

// CatalogEntry is one official-catalog model + its official reference prices as
// USD floats, plus whether a pricing_models row already exists for it.
type CatalogEntry struct {
	Model     string    `json:"model"`
	ModelType ModelType `json:"model_type"` // text | image
	Platform  string    `json:"platform"`   // anthropic | openai | gemini | other

	// Official reference prices (USD per token) for text models.
	InputPrice      float64 `json:"input_price"`
	OutputPrice     float64 `json:"output_price"`
	CacheReadPrice  float64 `json:"cache_read_price"`
	CacheWritePrice float64 `json:"cache_write_price"`

	// Official reference price (USD per image) for image models.
	ImagePrice float64 `json:"image_price"`

	// Added is true when a pricing_models row already exists for this model.
	Added bool `json:"added"`
}

// CatalogSyncResult is the small summary returned by SyncCatalog.
type CatalogSyncResult struct {
	Total int `json:"total"`
}

// CreateFromCatalogResult is the summary returned by CreateFromCatalog.
type CreateFromCatalogResult struct {
	Created int      `json:"created"`
	Skipped int      `json:"skipped"`
	Errors  []string `json:"errors,omitempty"`
}

// litellmProviderToCatalogPlatform maps a LiteLLM provider onto the admin
// pricing platform label. Unknown providers map to "other" (still listed).
func litellmProviderToCatalogPlatform(p string) string {
	switch strings.ToLower(strings.TrimSpace(p)) {
	case "anthropic":
		return "anthropic"
	case "openai":
		return "openai"
	case "google", "gemini":
		return "gemini"
	default:
		return "other"
	}
}

// litellmModeToModelType maps a LiteLLM mode onto the pricing model_type, or
// returns ("", false) for modes we don't bill (embedding / audio / …).
func litellmModeToModelType(mode string) (ModelType, bool) {
	switch mode {
	case "chat", "":
		return ModelTypeText, true
	case "image_generation":
		return ModelTypeImage, true
	default:
		return "", false
	}
}

// ListCatalog returns the official catalog entries, optionally filtered by
// platform, each flagged with whether a pricing_models row already exists.
// Prices are emitted as USD floats. Sorted by platform then model.
func (s *PricingDisplayService) ListCatalog(ctx context.Context, platform string) ([]CatalogEntry, error) {
	// Existing pricing_models names → the `added` flag. Match by model name
	// (case-insensitive) so a picked model greys out regardless of user_type.
	added := map[string]bool{}
	records, err := s.repo.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range records {
		added[strings.ToLower(r.Model)] = true
	}

	platformFilter := strings.ToLower(strings.TrimSpace(platform))

	entries := make([]CatalogEntry, 0)
	if s.pricing == nil {
		return entries, nil
	}

	for name, p := range s.pricing.ListAllPricing() {
		if p == nil {
			continue
		}
		plat := litellmProviderToCatalogPlatform(p.LiteLLMProvider)
		if platformFilter != "" && plat != platformFilter {
			continue
		}
		modelType, ok := litellmModeToModelType(p.Mode)
		if !ok {
			continue
		}
		entries = append(entries, CatalogEntry{
			Model:           name,
			ModelType:       modelType,
			Platform:        plat,
			InputPrice:      p.InputCostPerToken,
			OutputPrice:     p.OutputCostPerToken,
			CacheReadPrice:  p.CacheReadInputTokenCost,
			CacheWritePrice: p.CacheCreationInputTokenCost,
			ImagePrice:      p.OutputCostPerImage,
			Added:           added[strings.ToLower(name)],
		})
	}

	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Platform != entries[j].Platform {
			return entries[i].Platform < entries[j].Platform
		}
		return entries[i].Model < entries[j].Model
	})
	return entries, nil
}

// SyncCatalog refreshes the official prices from the remote source and returns
// the resulting model count.
func (s *PricingDisplayService) SyncCatalog(ctx context.Context) (CatalogSyncResult, error) {
	if s.pricing == nil {
		return CatalogSyncResult{}, nil
	}
	if err := s.pricing.ForceUpdate(); err != nil {
		return CatalogSyncResult{}, err
	}
	return CatalogSyncResult{Total: len(s.pricing.ListAllPricing())}, nil
}

// CreateFromCatalog creates a pricing_models row (enabled=false / 待启用) for
// each picked model, prefilling the official prices from the catalog. Models
// that already have a pricing_models row, or that aren't in the catalog, are
// skipped. Reuses the standard CreatePricingModel path so validation +
// saving_percent computation stay consistent.
func (s *PricingDisplayService) CreateFromCatalog(ctx context.Context, models []string) (CreateFromCatalogResult, error) {
	result := CreateFromCatalogResult{}

	// Existing pricing_models names → skip set (case-insensitive by name).
	existing := map[string]bool{}
	records, err := s.repo.List(ctx)
	if err != nil {
		return result, err
	}
	for _, r := range records {
		existing[strings.ToLower(r.Model)] = true
	}

	// De-dup the requested names (preserve first occurrence).
	seen := map[string]bool{}
	for _, raw := range models {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		key := strings.ToLower(name)
		if seen[key] {
			continue
		}
		seen[key] = true

		if existing[key] {
			result.Skipped++
			continue
		}

		if s.pricing == nil {
			result.Skipped++
			continue
		}
		p := s.pricing.GetModelPricing(name)
		if p == nil {
			result.Skipped++
			result.Errors = append(result.Errors, name+": no official pricing")
			continue
		}

		modelType, ok := litellmModeToModelType(p.Mode)
		if !ok {
			modelType = ModelTypeText
		}

		rec := &PricingModelRecord{
			Model:     name,
			ModelType: modelType,
			UserType:  catalogDefaultUserType,
			Enabled:   false, // 待启用: admin sets the real price then enables
		}
		if modelType == ModelTypeText {
			rec.InputPrice = floatPtr(p.InputCostPerToken)
			rec.OutputPrice = floatPtr(p.OutputCostPerToken)
			rec.CacheReadPrice = floatPtr(p.CacheReadInputTokenCost)
			rec.CacheWritePrice = floatPtr(p.CacheCreationInputTokenCost)
			rec.OfficialInputPrice = floatPtr(p.InputCostPerToken)
			rec.OfficialOutputPrice = floatPtr(p.OutputCostPerToken)
		}

		if _, err := s.CreatePricingModel(ctx, rec); err != nil {
			if errors.Is(err, ErrPricingModelExists) {
				// Raced against another create for the same model+user_type.
				result.Skipped++
				continue
			}
			result.Errors = append(result.Errors, name+": "+err.Error())
			continue
		}
		result.Created++
	}

	return result, nil
}

func floatPtr(v float64) *float64 {
	return &v
}
