package service

import (
	"context"
	"regexp"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// Region is the egress-region dictionary entry. It is the single source of
// truth for region display names: proxies store a Region.Code, and English /
// Chinese names are resolved from here (no per-proxy region_zh, no static map).
type Region struct {
	ID        int64
	Code      string // upper-case IATA-style code, e.g. SJC / NRT / ORD
	NameEn    string
	NameZh    string
	SortOrder int
	Enabled   bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Region errors.
var (
	ErrRegionNotFound   = infraerrors.NotFound("REGION_NOT_FOUND", "region not found")
	ErrRegionCodeExists = infraerrors.Conflict("REGION_CODE_EXISTS", "region code already exists")
	ErrRegionInvalid    = infraerrors.BadRequest("REGION_INVALID", "invalid region fields")
)

// regionCodePattern: 2–20 upper-case letters/digits (IATA-style). SelectProxy and
// proxy create/update normalise region to upper-case, so codes are stored upper.
var regionCodePattern = regexp.MustCompile(`^[A-Z0-9]{2,20}$`)

// RegionRepository is the persistence port for the region dictionary.
type RegionRepository interface {
	List(ctx context.Context, enabledOnly bool) ([]Region, error)
	GetByID(ctx context.Context, id int64) (*Region, error)
	CodeExists(ctx context.Context, code string) (bool, error)
	Create(ctx context.Context, in *Region) error
	Update(ctx context.Context, in *Region) error
	Delete(ctx context.Context, id int64) error
}

// RegionService manages the region dictionary (admin CRUD) and resolves display
// names for other services (available-regions, proxy display).
type RegionService struct {
	repo RegionRepository
}

// NewRegionService creates the region dictionary service.
func NewRegionService(repo RegionRepository) *RegionService {
	return &RegionService{repo: repo}
}

// NormalizeRegionCode trims + upper-cases a region code, matching the casing
// used by SelectProxy / normalizeProxyRegion so codes match end-to-end.
func NormalizeRegionCode(code string) string {
	return strings.ToUpper(strings.TrimSpace(code))
}

// List returns dictionary entries; enabledOnly hides disabled ones.
func (s *RegionService) List(ctx context.Context, enabledOnly bool) ([]Region, error) {
	return s.repo.List(ctx, enabledOnly)
}

// GetByID returns a single region.
func (s *RegionService) GetByID(ctx context.Context, id int64) (*Region, error) {
	return s.repo.GetByID(ctx, id)
}

// CreateRegionInput is the admin create payload.
type CreateRegionInput struct {
	Code      string
	NameEn    string
	NameZh    string
	SortOrder int
	Enabled   *bool
}

func (s *RegionService) Create(ctx context.Context, in CreateRegionInput) (*Region, error) {
	code := NormalizeRegionCode(in.Code)
	nameEn := strings.TrimSpace(in.NameEn)
	nameZh := strings.TrimSpace(in.NameZh)
	if !regionCodePattern.MatchString(code) || nameEn == "" || nameZh == "" {
		return nil, ErrRegionInvalid
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	r := &Region{
		Code:      code,
		NameEn:    nameEn,
		NameZh:    nameZh,
		SortOrder: in.SortOrder,
		Enabled:   enabled,
	}
	if err := s.repo.Create(ctx, r); err != nil {
		return nil, err
	}
	return r, nil
}

// UpdateRegionInput is the admin update payload. Nil fields are left unchanged.
type UpdateRegionInput struct {
	Code      *string
	NameEn    *string
	NameZh    *string
	SortOrder *int
	Enabled   *bool
}

func (s *RegionService) Update(ctx context.Context, id int64, in UpdateRegionInput) (*Region, error) {
	cur, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if in.Code != nil {
		cur.Code = NormalizeRegionCode(*in.Code)
	}
	if in.NameEn != nil {
		cur.NameEn = strings.TrimSpace(*in.NameEn)
	}
	if in.NameZh != nil {
		cur.NameZh = strings.TrimSpace(*in.NameZh)
	}
	if in.SortOrder != nil {
		cur.SortOrder = *in.SortOrder
	}
	if in.Enabled != nil {
		cur.Enabled = *in.Enabled
	}
	if !regionCodePattern.MatchString(cur.Code) || cur.NameEn == "" || cur.NameZh == "" {
		return nil, ErrRegionInvalid
	}
	if err := s.repo.Update(ctx, cur); err != nil {
		return nil, err
	}
	return cur, nil
}

func (s *RegionService) Delete(ctx context.Context, id int64) error {
	return s.repo.Delete(ctx, id)
}

// ResolveNames returns a map code → {en, zh} for the given codes, for callers
// that need to attach display names (e.g. available-regions). Unknown codes are
// simply absent from the map; the caller falls back to the raw code.
func (s *RegionService) ResolveNames(ctx context.Context) (map[string]Region, error) {
	rows, err := s.repo.List(ctx, false)
	if err != nil {
		return nil, err
	}
	m := make(map[string]Region, len(rows))
	for _, r := range rows {
		m[r.Code] = r
	}
	return m, nil
}
