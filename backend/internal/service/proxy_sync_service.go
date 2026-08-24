package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
)

// ProxyInput is one egress proxy the Portal wants present on this cell (multi-
// egress, Option A). Region defaults to the cell's region; MaxBindings defaults
// to 1 (one account per IP).
type ProxyInput struct {
	Protocol    string
	Host        string
	Port        int
	Username    string
	Password    string
	Region      string
	MaxBindings int
}

// ProxySyncResult reports what the sync changed (counts only — never creds).
type ProxySyncResult struct {
	Created  int `json:"created"`
	Updated  int `json:"updated"`
	Disabled int `json:"disabled"`
}

// ProxySyncService reconciles this cell's local `proxies` table with a desired
// set pushed from the Portal (Portal→cell /internal/proxies/sync). This is the
// only thing standing between the existing per-account allocator + multi-egress
// and a working IP pool: it fills the pool the allocator draws from.
type ProxySyncService struct {
	repo ProxyRepository
}

func NewProxySyncService(repo ProxyRepository) *ProxySyncService {
	return &ProxySyncService{repo: repo}
}

func proxyDedupKey(host string, port int, username string) string {
	return fmt.Sprintf("%s|%d|%s", strings.ToLower(strings.TrimSpace(host)), port, username)
}

// Sync upserts each desired proxy (keyed by host+port+username) as region-tagged,
// active, max_bindings-capped. In "replace" mode, active local proxies not in the
// desired set are DISABLED (status=disabled) — never hard-deleted — so accounts
// already bound to them keep working (account.Proxy.URL() ignores status) while
// the allocator (status='active' only) stops handing them out.
func (s *ProxySyncService) Sync(ctx context.Context, region string, desired []ProxyInput, mode string) (*ProxySyncResult, error) {
	// Load all local proxies (bounded page — a cell's pool is small).
	existing, _, err := s.repo.List(ctx, pagination.PaginationParams{Page: 1, PageSize: 1000})
	if err != nil {
		return nil, err
	}
	byKey := make(map[string]*Proxy, len(existing))
	for i := range existing {
		p := &existing[i]
		byKey[proxyDedupKey(p.Host, p.Port, p.Username)] = p
	}

	cellRegion := strings.ToUpper(strings.TrimSpace(region))
	res := &ProxySyncResult{}
	desiredKeys := make(map[string]struct{}, len(desired))

	for _, d := range desired {
		if strings.TrimSpace(d.Host) == "" || d.Port <= 0 {
			continue
		}
		key := proxyDedupKey(d.Host, d.Port, d.Username)
		desiredKeys[key] = struct{}{}

		protocol := strings.TrimSpace(d.Protocol)
		if protocol == "" {
			protocol = "socks5"
		}
		maxBind := d.MaxBindings
		if maxBind <= 0 {
			maxBind = 1
		}
		reg := cellRegion
		if r := strings.ToUpper(strings.TrimSpace(d.Region)); r != "" {
			reg = r
		}
		regPtr := &reg

		if p, ok := byKey[key]; ok {
			p.Protocol = protocol
			p.Password = d.Password
			p.MaxBindings = maxBind
			p.Status = "active"
			p.Region = regPtr
			if err := s.repo.Update(ctx, p); err != nil {
				return nil, err
			}
			res.Updated++
		} else {
			np := &Proxy{
				Name:           fmt.Sprintf("pool-%s-%d", d.Host, d.Port),
				Protocol:       protocol,
				Host:           d.Host,
				Port:           d.Port,
				Username:       d.Username,
				Password:       d.Password,
				Status:         "active",
				MaxBindings:    maxBind,
				Region:         regPtr,
				FallbackMode:   FallbackModeNone,
				ExpiryWarnDays: 7,
			}
			if err := s.repo.Create(ctx, np); err != nil {
				return nil, err
			}
			res.Created++
		}
	}

	if mode == "replace" {
		for i := range existing {
			p := &existing[i]
			if p.Status != "active" {
				continue
			}
			if _, ok := desiredKeys[proxyDedupKey(p.Host, p.Port, p.Username)]; ok {
				continue
			}
			p.Status = "disabled"
			if err := s.repo.Update(ctx, p); err != nil {
				return nil, err
			}
			res.Disabled++
		}
	}
	return res, nil
}
