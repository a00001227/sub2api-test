package service

import (
	"context"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// Phase 21F provider-account-deactivate: lets the Provider Portal stop and
// unbind an account it owns a reference to. Located by external_provider_
// account_id (Portal's pa_<uuid>). Deactivation = stop scheduling
// (schedulable=false) + mark disabled, so the account leaves the scheduling
// pool immediately. Historical usage/billing rows are NOT touched.
//
// Capacity is reclaimed implicitly: proxy availability is computed from the
// live "active account count < max_bindings", so a disabled account drops out
// of that count and its proxy returns to the pool — no explicit release needed
// (see ReleaseAllocationByExternalRef's note).
//
// Idempotent: an unknown ref or an already-disabled account is a benign
// success, so the Portal can retry safely.

// providerDeactivateLocator resolves an account id from the Portal's ref.
type providerDeactivateLocator interface {
	FindAccountIDByExternalRef(ctx context.Context, externalRef string) (int64, bool, error)
}

// reasonPortalRollback marks a deactivate call as a rollback of a just-minted
// account whose Portal row failed to commit. Such an account was never bound to
// anything, so it is HARD-DELETED rather than left as a disabled/error orphan
// (which would occupy nothing but be health-probed forever and pollute usage
// reflow with unknown-ref events). Must match the Portal's rollback reason string.
const reasonPortalRollback = "portal_rollback"

// providerDeactivateAccountRepo reads and mutates the account row. Satisfied by
// the concrete account repository (GetByID + SetSchedulable + Update + Delete).
type providerDeactivateAccountRepo interface {
	GetByID(ctx context.Context, id int64) (*Account, error)
	SetSchedulable(ctx context.Context, id int64, schedulable bool) error
	Update(ctx context.Context, account *Account) error
	Delete(ctx context.Context, id int64) error
}

// ProviderAccountDeactivationResult is the outcome returned to the Portal.
type ProviderAccountDeactivationResult struct {
	Status string `json:"status"` // "deactivated" | "deleted" | "not_found" | "already_inactive"
}

// ProviderAccountDeactivationService deactivates a provider-owned account.
type ProviderAccountDeactivationService struct {
	locator providerDeactivateLocator
	repo    providerDeactivateAccountRepo
}

// NewProviderAccountDeactivationService builds the service.
func NewProviderAccountDeactivationService(
	locator providerDeactivateLocator,
	repo providerDeactivateAccountRepo,
) *ProviderAccountDeactivationService {
	return &ProviderAccountDeactivationService{locator: locator, repo: repo}
}

// Deactivate stops scheduling for the account referenced by externalRef and
// marks it disabled. Idempotent.
func (s *ProviderAccountDeactivationService) Deactivate(
	ctx context.Context, externalRef, reason string,
) (*ProviderAccountDeactivationResult, error) {
	externalRef = strings.TrimSpace(externalRef)
	if externalRef == "" || !strings.HasPrefix(externalRef, "pa_") {
		return nil, infraerrors.BadRequest("INVALID_REQUEST", "invalid external_provider_account_id")
	}

	id, found, err := s.locator.FindAccountIDByExternalRef(ctx, externalRef)
	if err != nil {
		return nil, err
	}
	if !found {
		// Unknown ref → benign no-op so the Portal can retry / stay in sync.
		return &ProviderAccountDeactivationResult{Status: "not_found"}, nil
	}

	// Rollback of a just-minted account (Portal failed to persist its row after
	// the cell minted the token): hard-delete it. Leaving it disabled/error would
	// strand an orphan on the cell — no Portal row to ever settle its usage, yet
	// the scheduled-test-runner keeps probing it (unknown-ref reflow noise) and it
	// counts against nothing but clutters the pool. Delete cleans the account,
	// its scheduler snapshot, and test plans in one shot. Idempotent: a repeated
	// rollback finds no ref (handled above).
	if strings.TrimSpace(reason) == reasonPortalRollback {
		if err := s.repo.Delete(ctx, id); err != nil {
			return nil, err
		}
		return &ProviderAccountDeactivationResult{Status: "deleted"}, nil
	}

	acc, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	// Already disabled → idempotent success, don't re-write.
	if acc != nil && acc.Status == domain.StatusDisabled {
		return &ProviderAccountDeactivationResult{Status: "already_inactive"}, nil
	}

	// Stop scheduling first, then flip status. Either failing surfaces an error
	// (the Portal keeps the account and can retry).
	if err := s.repo.SetSchedulable(ctx, id, false); err != nil {
		return nil, err
	}
	if acc != nil {
		acc.Status = domain.StatusDisabled
		acc.ErrorMessage = ""
		if err := s.repo.Update(ctx, acc); err != nil {
			return nil, err
		}
	}
	_ = reason // reserved for future audit; never logged with credential data
	return &ProviderAccountDeactivationResult{Status: "deactivated"}, nil
}
