package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
)

// syncProxyRepoStub tracks a local proxies table for ProxySyncService tests.
type syncProxyRepoStub struct {
	proxies []Proxy
	nextID  int64
}

func (s *syncProxyRepoStub) List(_ context.Context, _ pagination.PaginationParams) ([]Proxy, *pagination.PaginationResult, error) {
	out := make([]Proxy, len(s.proxies))
	copy(out, s.proxies)
	return out, nil, nil
}
func (s *syncProxyRepoStub) Create(_ context.Context, p *Proxy) error {
	s.nextID++
	p.ID = s.nextID
	s.proxies = append(s.proxies, *p)
	return nil
}
func (s *syncProxyRepoStub) Update(_ context.Context, p *Proxy) error {
	for i := range s.proxies {
		if s.proxies[i].ID == p.ID {
			s.proxies[i] = *p
		}
	}
	return nil
}
func (s *syncProxyRepoStub) GetByID(context.Context, int64) (*Proxy, error) { return nil, nil }
func (s *syncProxyRepoStub) ListByIDs(context.Context, []int64) ([]Proxy, error) {
	return nil, nil
}
func (s *syncProxyRepoStub) Delete(context.Context, int64) error { return nil }
func (s *syncProxyRepoStub) ListWithFilters(context.Context, pagination.PaginationParams, string, string, string) ([]Proxy, *pagination.PaginationResult, error) {
	return nil, nil, nil
}
func (s *syncProxyRepoStub) ListWithFiltersAndAccountCount(context.Context, pagination.PaginationParams, string, string, string) ([]ProxyWithAccountCount, *pagination.PaginationResult, error) {
	return nil, nil, nil
}
func (s *syncProxyRepoStub) ListActive(context.Context) ([]Proxy, error) { return nil, nil }
func (s *syncProxyRepoStub) ListActiveWithAccountCount(context.Context) ([]ProxyWithAccountCount, error) {
	return nil, nil
}
func (s *syncProxyRepoStub) ExistsByHostPortAuth(context.Context, string, int, string, string) (bool, error) {
	return false, nil
}
func (s *syncProxyRepoStub) CountAccountsByProxyID(context.Context, int64) (int64, error) {
	return 0, nil
}
func (s *syncProxyRepoStub) ListAccountSummariesByProxyID(context.Context, int64) ([]ProxyAccountSummary, error) {
	return nil, nil
}
func (s *syncProxyRepoStub) SweepExpiredProxies(context.Context, time.Time) (int64, error) {
	return 0, nil
}
func (s *syncProxyRepoStub) ListAllForFallback(context.Context) ([]Proxy, error) { return nil, nil }
func (s *syncProxyRepoStub) CountExpired(context.Context) (int64, error)         { return 0, nil }
func (s *syncProxyRepoStub) CountExpiringSoon(context.Context, time.Time) (int64, error) {
	return 0, nil
}

func in(protocol, host string, port int, user, pass string) ProxyInput {
	return ProxyInput{Protocol: protocol, Host: host, Port: port, Username: user, Password: pass}
}

func TestProxySync_CreateThenUpdate(t *testing.T) {
	repo := &syncProxyRepoStub{}
	svc := NewProxySyncService(repo)
	ctx := context.Background()

	// First sync: 2 fresh IPs → both created, region=cell region, max_bindings=1.
	res, err := svc.Sync(ctx, "bom", []ProxyInput{
		in("socks5", "1.1.1.1", 443, "u1", "p1"),
		in("socks5", "2.2.2.2", 443, "u2", "p2"),
	}, "upsert")
	if err != nil {
		t.Fatal(err)
	}
	if res.Created != 2 || res.Updated != 0 {
		t.Fatalf("first sync: created=%d updated=%d", res.Created, res.Updated)
	}
	if len(repo.proxies) != 2 {
		t.Fatalf("want 2 proxies, got %d", len(repo.proxies))
	}
	for _, p := range repo.proxies {
		if p.Region == nil || *p.Region != "BOM" {
			t.Fatalf("region not set to BOM: %+v", p.Region)
		}
		if p.MaxBindings != 1 || p.Status != "active" {
			t.Fatalf("want max_bindings=1 active, got %d %s", p.MaxBindings, p.Status)
		}
	}

	// Second sync: same (host,port,user) with a NEW password → update, not create.
	res, err = svc.Sync(ctx, "bom", []ProxyInput{in("socks5", "1.1.1.1", 443, "u1", "NEWPASS")}, "upsert")
	if err != nil {
		t.Fatal(err)
	}
	if res.Created != 0 || res.Updated != 1 {
		t.Fatalf("second sync: created=%d updated=%d", res.Created, res.Updated)
	}
	// upsert mode: the other proxy (2.2.2.2) must NOT be disabled.
	for _, p := range repo.proxies {
		if p.Host == "2.2.2.2" && p.Status != "active" {
			t.Fatalf("upsert should not disable others; 2.2.2.2 = %s", p.Status)
		}
		if p.Host == "1.1.1.1" && p.Password != "NEWPASS" {
			t.Fatalf("password not updated: %q", p.Password)
		}
	}
}

func TestProxySync_ReplaceDisablesMissing(t *testing.T) {
	repo := &syncProxyRepoStub{
		proxies: []Proxy{
			{ID: 1, Host: "1.1.1.1", Port: 443, Username: "u1", Status: "active"},
			{ID: 2, Host: "2.2.2.2", Port: 443, Username: "u2", Status: "active"},
		},
		nextID: 2,
	}
	svc := NewProxySyncService(repo)

	// replace with only 1.1.1.1 → 2.2.2.2 must be disabled (not deleted).
	res, err := svc.Sync(context.Background(), "bom",
		[]ProxyInput{in("socks5", "1.1.1.1", 443, "u1", "p1")}, "replace")
	if err != nil {
		t.Fatal(err)
	}
	if res.Disabled != 1 {
		t.Fatalf("want 1 disabled, got %d", res.Disabled)
	}
	if len(repo.proxies) != 2 {
		t.Fatalf("replace must not delete; want 2 rows, got %d", len(repo.proxies))
	}
	for _, p := range repo.proxies {
		if p.Host == "2.2.2.2" && p.Status != "disabled" {
			t.Fatalf("2.2.2.2 should be disabled, got %s", p.Status)
		}
	}
}
