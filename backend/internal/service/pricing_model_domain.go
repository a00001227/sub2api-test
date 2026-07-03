package service

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// ModelType defines the billing type of a pricing model.
type ModelType string

const (
	ModelTypeText  ModelType = "text"
	ModelTypeImage ModelType = "image"
)

// UserType distinguishes end-user vs channel-user pricing tiers.
type UserType string

const (
	UserTypeEndUser     UserType = "end_user"
	UserTypeChannelUser UserType = "channel_user"
)

// PricingModelRecord is the domain object for a row in pricing_models.
type PricingModelRecord struct {
	ID        int64
	Model     string
	ModelType ModelType
	UserType  UserType
	Enabled   bool

	// Text fields (nil for image models)
	InputPrice          *float64
	OutputPrice         *float64
	CacheReadPrice      *float64
	CacheWritePrice     *float64
	OfficialInputPrice  *float64
	OfficialOutputPrice *float64

	// Image field: raw JSON string e.g. `{"1k":0.005,"2k":0.01}`
	ImagePricingJSON *string

	// Computed by service layer
	SavingPercent float64
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// ImagePricing parses ImagePricingJSON into a map.
func (r *PricingModelRecord) ImagePricing() (map[string]float64, error) {
	if r.ImagePricingJSON == nil || *r.ImagePricingJSON == "" {
		return map[string]float64{}, nil
	}
	var m map[string]float64
	if err := json.Unmarshal([]byte(*r.ImagePricingJSON), &m); err != nil {
		return nil, fmt.Errorf("parse image_pricing_json: %w", err)
	}
	return m, nil
}

// PricingModelRepository is the data access interface for pricing_models.
type PricingModelRepository interface {
	Create(ctx context.Context, r *PricingModelRecord) error
	Update(ctx context.Context, r *PricingModelRecord) error
	Delete(ctx context.Context, id int64) error
	GetByID(ctx context.Context, id int64) (*PricingModelRecord, error)
	List(ctx context.Context) ([]*PricingModelRecord, error)
	ListEnabled(ctx context.Context) ([]*PricingModelRecord, error)
}

// ---- Sentinel errors ----

var ErrPricingModelNotFound = fmt.Errorf("pricing model not found")
var ErrPricingModelExists = fmt.Errorf("pricing model already exists for that model+user_type")
