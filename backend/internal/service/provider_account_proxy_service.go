package service

import (
	"context"
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// ProviderAccountProxyService re-binds an existing provider account to a proxy
// the Portal has already synced into THIS cell's local proxies table, located by
// that proxy's unique region code (the owned-proxy convention OWN-<hex>).
//
// This is the cell-side half of the admin "change proxy IP" feature. The Portal
// first creates+probes+syncs a brand-new owned proxy (unique region,
// max_bindings=1); then it calls this endpoint to point the account's proxy_id at
// it. Exclusivity ("no other account can grab it") is structural: the cell's
// allocator selects proxies by EXACT region equality, and no other account
// carries this proxy's unique region — so it is never handed out elsewhere. We
// therefore deliberately do NOT run the per-platform capacity guard here (that
// guard, on an idempotent re-run, would miscount this very account's own slot as
// PROXY_CAPACITY_FULL); the region uniqueness already guarantees isolation.
//
// Idempotent: re-binding to the same region re-points proxy_id at the same id.
type ProviderAccountProxyService struct {
	locator     providerDeactivateLocator // FindAccountIDByExternalRef
	proxies     proxyRegionResolver
	accountRepo providerProxyAccountRepo
}

// proxyRegionResolver resolves a synced proxy's local id by its unique region.
type proxyRegionResolver interface {
	FindActiveProxyIDByRegion(ctx context.Context, region string) (int64, bool, error)
}

// providerProxyAccountRepo reads and mutates the account row (GetByID + Update),
// mirroring the deactivation service's minimal surface.
type providerProxyAccountRepo interface {
	GetByID(ctx context.Context, id int64) (*Account, error)
	Update(ctx context.Context, account *Account) error
}

// ProviderAccountProxyResult is the outcome returned to the Portal.
type ProviderAccountProxyResult struct {
	Status  string `json:"status"` // "bound"
	ProxyID int64  `json:"proxy_id"`
}

// NewProviderAccountProxyService builds the service.
func NewProviderAccountProxyService(
	locator providerDeactivateLocator,
	proxies proxyRegionResolver,
	accountRepo providerProxyAccountRepo,
) *ProviderAccountProxyService {
	return &ProviderAccountProxyService{locator: locator, proxies: proxies, accountRepo: accountRepo}
}

// SetProxyByRegion binds the account referenced by externalRef to the proxy whose
// unique region matches. Errors (not a benign no-op) when the account or the
// proxy can't be found: unlike deactivate, a missing target here means the
// requested re-bind did NOT happen, and the Portal must not persist its mirror.
func (s *ProviderAccountProxyService) SetProxyByRegion(
	ctx context.Context, externalRef, region string,
) (*ProviderAccountProxyResult, error) {
	externalRef = strings.TrimSpace(externalRef)
	if externalRef == "" || !strings.HasPrefix(externalRef, "pa_") {
		return nil, infraerrors.BadRequest("INVALID_REQUEST", "invalid external_provider_account_id")
	}
	region = strings.TrimSpace(region)
	if region == "" {
		return nil, infraerrors.BadRequest("INVALID_REQUEST", "region is required")
	}

	id, found, err := s.locator.FindAccountIDByExternalRef(ctx, externalRef)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, infraerrors.NotFound("ACCOUNT_NOT_FOUND", "provider account not found")
	}

	proxyID, found, err := s.proxies.FindActiveProxyIDByRegion(ctx, region)
	if err != nil {
		return nil, err
	}
	if !found {
		// The Portal must sync the proxy into this cell BEFORE binding; if it's
		// not here, the bind can't proceed — surface it so the Portal rolls back.
		return nil, infraerrors.Conflict("PROXY_NOT_SYNCED", "target proxy not found on this cell")
	}

	acc, err := s.accountRepo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if acc == nil {
		return nil, infraerrors.NotFound("ACCOUNT_NOT_FOUND", "provider account not found")
	}

	// Re-point proxy_id (GetByID → mutate → Update round-trips every other field
	// unchanged, exactly like the deactivation service's status flip).
	acc.ProxyID = &proxyID
	acc.Proxy = nil
	if err := s.accountRepo.Update(ctx, acc); err != nil {
		return nil, err
	}
	return &ProviderAccountProxyResult{Status: "bound", ProxyID: proxyID}, nil
}
