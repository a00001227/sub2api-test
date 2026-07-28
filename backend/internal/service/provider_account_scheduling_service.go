package service

import (
	"context"
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// Phase 21I provider-account-scheduling: lets the Provider Portal PAUSE and
// RESUME an account it owns a reference to, reversibly. Located by external_
// provider_account_id (Portal's pa_<uuid>).
//
// Unlike Deactivate (Phase 21F) — which stops scheduling AND flips status to
// disabled as a terminal unbind — pausing only toggles the `schedulable`
// boolean and never touches `status`. That keeps it fully reversible: resume =
// set schedulable back to true. Status stays "active" throughout, matching
// sub2api's design where transient/reversible scheduling changes don't write
// the status column (rate-limit / overload use timestamps, admin uses the same
// schedulable toggle).
//
// Idempotent: an unknown ref, or an account already at the requested schedulable
// value, is a benign success so the Portal can retry / stay in sync.

// providerSchedulingLocator resolves an account id from the Portal's ref.
type providerSchedulingLocator interface {
	FindAccountIDByExternalRef(ctx context.Context, externalRef string) (int64, bool, error)
}

// providerSchedulingAccountRepo reads and toggles the schedulable flag.
type providerSchedulingAccountRepo interface {
	GetByID(ctx context.Context, id int64) (*Account, error)
	SetSchedulable(ctx context.Context, id int64, schedulable bool) error
}

// ProviderAccountSchedulingResult is the outcome returned to the Portal.
type ProviderAccountSchedulingResult struct {
	Status string `json:"status"` // "updated" | "not_found" | "unchanged"
}

// ProviderAccountSchedulingService pauses/resumes a provider-owned account.
type ProviderAccountSchedulingService struct {
	locator providerSchedulingLocator
	repo    providerSchedulingAccountRepo
}

// NewProviderAccountSchedulingService builds the service.
func NewProviderAccountSchedulingService(
	locator providerSchedulingLocator,
	repo providerSchedulingAccountRepo,
) *ProviderAccountSchedulingService {
	return &ProviderAccountSchedulingService{locator: locator, repo: repo}
}

// SetScheduling toggles whether the account referenced by externalRef takes new
// scheduling. enabled=false pauses (schedulable=false), enabled=true resumes.
// Never touches status. Idempotent.
func (s *ProviderAccountSchedulingService) SetScheduling(
	ctx context.Context, externalRef string, enabled bool,
) (*ProviderAccountSchedulingResult, error) {
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
		return &ProviderAccountSchedulingResult{Status: "not_found"}, nil
	}

	acc, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	// Already at the requested value → idempotent success, don't re-write.
	if acc != nil && acc.Schedulable == enabled {
		return &ProviderAccountSchedulingResult{Status: "unchanged"}, nil
	}

	if err := s.repo.SetSchedulable(ctx, id, enabled); err != nil {
		return nil, err
	}
	return &ProviderAccountSchedulingResult{Status: "updated"}, nil
}
