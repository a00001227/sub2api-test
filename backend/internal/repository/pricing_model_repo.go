package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

type pricingModelRepository struct {
	db *sql.DB
}

// NewPricingModelRepository creates a pricing_models data access instance.
func NewPricingModelRepository(db *sql.DB) service.PricingModelRepository {
	return &pricingModelRepository{db: db}
}

const pricingModelSelectCols = `
    id, model, model_type, user_type, enabled,
    input_price, output_price, cache_read_price, cache_write_price,
    official_input_price, official_output_price,
    image_pricing_json,
    saving_percent, created_at, updated_at`

func scanPricingModel(row interface {
	Scan(dest ...any) error
}) (*service.PricingModelRecord, error) {
	r := &service.PricingModelRecord{}
	var modelType, userType string
	err := row.Scan(
		&r.ID, &r.Model, &modelType, &userType, &r.Enabled,
		&r.InputPrice, &r.OutputPrice, &r.CacheReadPrice, &r.CacheWritePrice,
		&r.OfficialInputPrice, &r.OfficialOutputPrice,
		&r.ImagePricingJSON,
		&r.SavingPercent, &r.CreatedAt, &r.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	r.ModelType = service.ModelType(modelType)
	r.UserType = service.UserType(userType)
	return r, nil
}

func (r *pricingModelRepository) Create(ctx context.Context, rec *service.PricingModelRecord) error {
	row := r.db.QueryRowContext(ctx, `
		INSERT INTO pricing_models
		    (model, model_type, user_type, enabled,
		     input_price, output_price, cache_read_price, cache_write_price,
		     official_input_price, official_output_price,
		     image_pricing_json, saving_percent, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, NOW(), NOW())
		RETURNING `+pricingModelSelectCols,
		rec.Model, string(rec.ModelType), string(rec.UserType), rec.Enabled,
		rec.InputPrice, rec.OutputPrice, rec.CacheReadPrice, rec.CacheWritePrice,
		rec.OfficialInputPrice, rec.OfficialOutputPrice,
		rec.ImagePricingJSON, rec.SavingPercent,
	)
	result, err := scanPricingModel(row)
	if err != nil {
		if isPricingUniqueViolation(err) {
			return service.ErrPricingModelExists
		}
		return fmt.Errorf("insert pricing_model: %w", err)
	}
	*rec = *result
	return nil
}

func (r *pricingModelRepository) Update(ctx context.Context, rec *service.PricingModelRecord) error {
	row := r.db.QueryRowContext(ctx, `
		UPDATE pricing_models SET
		    model                = $1,
		    model_type           = $2,
		    user_type            = $3,
		    enabled              = $4,
		    input_price          = $5,
		    output_price         = $6,
		    cache_read_price     = $7,
		    cache_write_price    = $8,
		    official_input_price = $9,
		    official_output_price= $10,
		    image_pricing_json   = $11,
		    saving_percent       = $12,
		    updated_at           = NOW()
		WHERE id = $13
		RETURNING `+pricingModelSelectCols,
		rec.Model, string(rec.ModelType), string(rec.UserType), rec.Enabled,
		rec.InputPrice, rec.OutputPrice, rec.CacheReadPrice, rec.CacheWritePrice,
		rec.OfficialInputPrice, rec.OfficialOutputPrice,
		rec.ImagePricingJSON, rec.SavingPercent, rec.ID,
	)
	result, err := scanPricingModel(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return service.ErrPricingModelNotFound
		}
		if isPricingUniqueViolation(err) {
			return service.ErrPricingModelExists
		}
		return fmt.Errorf("update pricing_model: %w", err)
	}
	*rec = *result
	return nil
}

func (r *pricingModelRepository) Delete(ctx context.Context, id int64) error {
	res, err := r.db.ExecContext(ctx, `DELETE FROM pricing_models WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete pricing_model: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return service.ErrPricingModelNotFound
	}
	return nil
}

func (r *pricingModelRepository) GetByID(ctx context.Context, id int64) (*service.PricingModelRecord, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT `+pricingModelSelectCols+` FROM pricing_models WHERE id = $1`, id)
	rec, err := scanPricingModel(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, service.ErrPricingModelNotFound
		}
		return nil, fmt.Errorf("get pricing_model by id: %w", err)
	}
	return rec, nil
}

func (r *pricingModelRepository) List(ctx context.Context) ([]*service.PricingModelRecord, error) {
	return r.query(ctx, `SELECT `+pricingModelSelectCols+`
		FROM pricing_models ORDER BY model ASC, user_type ASC`)
}

func (r *pricingModelRepository) ListEnabled(ctx context.Context) ([]*service.PricingModelRecord, error) {
	return r.query(ctx, `SELECT `+pricingModelSelectCols+`
		FROM pricing_models WHERE enabled = TRUE ORDER BY model ASC, user_type ASC`)
}

func (r *pricingModelRepository) query(ctx context.Context, q string, args ...any) ([]*service.PricingModelRecord, error) {
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query pricing_models: %w", err)
	}
	defer rows.Close()

	var records []*service.PricingModelRecord
	for rows.Next() {
		rec, err := scanPricingModel(rows)
		if err != nil {
			return nil, fmt.Errorf("scan pricing_model: %w", err)
		}
		records = append(records, rec)
	}
	return records, rows.Err()
}

func isPricingUniqueViolation(err error) bool {
	var pqErr *pq.Error
	if errors.As(err, &pqErr) {
		return pqErr.Code == "23505"
	}
	return false
}

// ensure compile-time compatibility with time.Time usage
var _ = time.Now
