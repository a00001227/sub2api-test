package repository

import (
	"context"
	"strings"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	dbregion "github.com/Wei-Shaw/sub2api/ent/region"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

type regionRepository struct {
	client *dbent.Client
}

// NewRegionRepository creates the egress-region dictionary repository.
func NewRegionRepository(client *dbent.Client) service.RegionRepository {
	return &regionRepository{client: client}
}

func regionEntityToService(m *dbent.Region) *service.Region {
	if m == nil {
		return nil
	}
	return &service.Region{
		ID:        m.ID,
		Code:      m.Code,
		NameEn:    m.NameEn,
		NameZh:    m.NameZh,
		SortOrder: m.SortOrder,
		Enabled:   m.Enabled,
		CreatedAt: m.CreatedAt,
		UpdatedAt: m.UpdatedAt,
	}
}

// List returns all regions ordered by sort_order asc, then code. When
// enabledOnly is true, disabled regions are excluded.
func (r *regionRepository) List(ctx context.Context, enabledOnly bool) ([]service.Region, error) {
	q := r.client.Region.Query()
	if enabledOnly {
		q = q.Where(dbregion.EnabledEQ(true))
	}
	rows, err := q.
		Order(dbent.Asc(dbregion.FieldSortOrder), dbent.Asc(dbregion.FieldCode)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]service.Region, 0, len(rows))
	for _, m := range rows {
		out = append(out, *regionEntityToService(m))
	}
	return out, nil
}

func (r *regionRepository) GetByID(ctx context.Context, id int64) (*service.Region, error) {
	m, err := r.client.Region.Query().Where(dbregion.IDEQ(id)).Only(ctx)
	if err != nil {
		if dbent.IsNotFound(err) {
			return nil, service.ErrRegionNotFound
		}
		return nil, err
	}
	return regionEntityToService(m), nil
}

// CodeExists reports whether a region with the given code exists (case-sensitive;
// callers normalise to upper-case first).
func (r *regionRepository) CodeExists(ctx context.Context, code string) (bool, error) {
	return r.client.Region.Query().Where(dbregion.CodeEQ(code)).Exist(ctx)
}

func (r *regionRepository) Create(ctx context.Context, in *service.Region) error {
	m, err := r.client.Region.Create().
		SetCode(in.Code).
		SetNameEn(in.NameEn).
		SetNameZh(in.NameZh).
		SetSortOrder(in.SortOrder).
		SetEnabled(in.Enabled).
		Save(ctx)
	if err != nil {
		if isUniqueConstraintErr(err) {
			return service.ErrRegionCodeExists
		}
		return err
	}
	*in = *regionEntityToService(m)
	return nil
}

func (r *regionRepository) Update(ctx context.Context, in *service.Region) error {
	m, err := r.client.Region.UpdateOneID(in.ID).
		SetCode(in.Code).
		SetNameEn(in.NameEn).
		SetNameZh(in.NameZh).
		SetSortOrder(in.SortOrder).
		SetEnabled(in.Enabled).
		Save(ctx)
	if err != nil {
		if dbent.IsNotFound(err) {
			return service.ErrRegionNotFound
		}
		if isUniqueConstraintErr(err) {
			return service.ErrRegionCodeExists
		}
		return err
	}
	*in = *regionEntityToService(m)
	return nil
}

func (r *regionRepository) Delete(ctx context.Context, id int64) error {
	err := r.client.Region.DeleteOneID(id).Exec(ctx)
	if err != nil {
		if dbent.IsNotFound(err) {
			return service.ErrRegionNotFound
		}
		return err
	}
	return nil
}

// isUniqueConstraintErr detects a Postgres unique-violation surfaced through ent.
func isUniqueConstraintErr(err error) bool {
	return err != nil && (dbent.IsConstraintError(err) ||
		strings.Contains(strings.ToLower(err.Error()), "unique"))
}
