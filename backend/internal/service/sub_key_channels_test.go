package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

// 覆盖 PUT /sub-keys/:id 的通道集合变更链路（service 层）：
// GroupID + AllowedGroupIDs 提供时，主通道与白名单应整体重解析后落库。

type channelsKeyRepoStub struct {
	APIKeyRepository // 未覆盖的方法调用即 panic（nil 接口）

	key *APIKey

	gotBudgetWrite bool
	gotGroupID     *int64
	gotAllowed     []int64
}

func (s *channelsKeyRepoStub) GetSubKeyByIDForUser(ctx context.Context, userID, subKeyID int64) (*APIKey, error) {
	if s.key == nil || s.key.UserID != userID || s.key.ID != subKeyID {
		return nil, ErrAPIKeyNotFound
	}
	cp := *s.key
	return &cp, nil
}

func (s *channelsKeyRepoStub) UpdateSubKeyBudget(ctx context.Context, id int64, name string, quota, displayMultiplier float64, status string) error {
	s.gotBudgetWrite = true
	return nil
}

func (s *channelsKeyRepoStub) UpdateSubKeyChannels(ctx context.Context, id int64, groupID *int64, groupIDs []int64) error {
	s.gotGroupID = groupID
	s.gotAllowed = groupIDs
	// 模拟落库，让末尾的 re-read 返回新值
	s.key.GroupID = groupID
	s.key.AllowedGroupIDs = groupIDs
	return nil
}

type channelsUserRepoStub struct {
	UserRepository
	user *User
}

func (s *channelsUserRepoStub) GetByID(ctx context.Context, id int64) (*User, error) {
	return s.user, nil
}

type channelsGroupRepoStub struct {
	GroupRepository
	groups map[int64]*Group
}

func (s *channelsGroupRepoStub) GetByID(ctx context.Context, id int64) (*Group, error) {
	g, ok := s.groups[id]
	if !ok {
		return nil, ErrGroupNotFound
	}
	return g, nil
}

func (s *channelsGroupRepoStub) ListActive(ctx context.Context) ([]Group, error) {
	out := make([]Group, 0, len(s.groups))
	for _, g := range s.groups {
		out = append(out, *g)
	}
	return out, nil
}

// 无订阅的用户桩(GetAvailableGroups 会查订阅)。
type channelsUserSubRepoStub struct{ UserSubscriptionRepository }

func (channelsUserSubRepoStub) ListActiveByUserID(context.Context, int64) ([]UserSubscription, error) {
	return nil, nil
}

// 客户密钥未指定通道时,默认放开面向客户的固定组(proxy+openai);内部号池组(蒸馏)
// 既不在白名单也不进默认。护号 UX 的核心断言。
func TestResolveSubKeyGroupsDefaultsToCustomerFacingChannels(t *testing.T) {
	proxy, openai, distill, main := int64(1), int64(2), int64(3), int64(9)
	mk := func(id int64, slug string) *Group {
		return &Group{ID: id, Name: "g", Slug: slug, Platform: "anthropic", Status: StatusActive, Hydrated: true, SubscriptionType: "standard"}
	}
	groupRepo := &channelsGroupRepoStub{groups: map[int64]*Group{
		proxy:   mk(proxy, "proxy"),
		openai:  mk(openai, "openai"),
		distill: mk(distill, "distill"), // 内部蒸馏组,slug 不在默认白名单
		main:    mk(main, "vip"),
	}}
	svc := NewAPIKeyService(
		&channelsKeyRepoStub{},
		&channelsUserRepoStub{user: &User{ID: 7, Balance: 1000}},
		groupRepo,
		channelsUserSubRepoStub{},
		nil, nil,
		&config.Config{},
	)

	mainID := main
	// opts 完全不带通道 → 走默认。
	gotMain, allowed, err := svc.resolveSubKeyGroups(context.Background(), &User{ID: 7}, &mainID, CreateSubKeyOptions{})
	if err != nil {
		t.Fatalf("resolveSubKeyGroups: %v", err)
	}
	if gotMain == nil || *gotMain != main {
		t.Fatalf("main group = %v, want %d", gotMain, main)
	}
	set := map[int64]bool{}
	for _, id := range allowed {
		set[id] = true
	}
	if len(allowed) != 2 || !set[proxy] || !set[openai] {
		t.Fatalf("默认通道应为 proxy+openai; got %v", allowed)
	}
	if set[distill] {
		t.Fatalf("内部蒸馏组绝不应进入客户密钥默认通道; got %v", allowed)
	}

	// 显式传了通道 → 不触发默认(只放开显式那个)。
	_, allowed2, err := svc.resolveSubKeyGroups(context.Background(), &User{ID: 7}, &mainID, CreateSubKeyOptions{AllowedGroupIDs: []int64{proxy}})
	if err != nil {
		t.Fatalf("resolveSubKeyGroups(explicit): %v", err)
	}
	if len(allowed2) != 1 || allowed2[0] != proxy {
		t.Fatalf("显式指定应原样保留、不追加默认; got %v", allowed2)
	}
}

func TestUpdateSubKeyChangesChannels(t *testing.T) {
	mainOld, chanB, chanC := int64(1), int64(2), int64(3)
	mkGroup := func(id int64) *Group {
		return &Group{ID: id, Name: "g", Platform: "anthropic", Status: StatusActive, Hydrated: true, SubscriptionType: "standard"}
	}
	keyRepo := &channelsKeyRepoStub{
		key: &APIKey{
			ID:          10,
			UserID:      7,
			Key:         "sk-sub",
			Name:        "customer",
			ParentKeyID: ptrInt64(99),
			GroupID:     &mainOld,
			Quota:       20,
			QuotaUsed:   5,
			Status:      StatusAPIKeyActive,
			CreatedAt:   time.Now(),
		},
	}
	svc := NewAPIKeyService(
		keyRepo,
		&channelsUserRepoStub{user: &User{ID: 7, Balance: 1000}},
		&channelsGroupRepoStub{groups: map[int64]*Group{mainOld: mkGroup(mainOld), chanB: mkGroup(chanB), chanC: mkGroup(chanC)}},
		nil, nil, nil,
		&config.Config{},
	)

	accountKey := &APIKey{ID: 99, UserID: 7}
	gid := mainOld
	updated, err := svc.UpdateSubKey(context.Background(), accountKey, 10, UpdateSubKeyRequest{
		GroupID:         &gid,
		AllowedGroupIDs: &[]int64{chanB, chanC},
	})
	if err != nil {
		t.Fatalf("UpdateSubKey: %v", err)
	}
	if keyRepo.gotGroupID == nil || *keyRepo.gotGroupID != mainOld {
		t.Fatalf("main group write = %v, want %d", keyRepo.gotGroupID, mainOld)
	}
	if len(keyRepo.gotAllowed) != 2 || keyRepo.gotAllowed[0] != chanB || keyRepo.gotAllowed[1] != chanC {
		t.Fatalf("allowed write = %v, want [%d %d]", keyRepo.gotAllowed, chanB, chanC)
	}
	if updated.GroupID == nil || *updated.GroupID != mainOld || len(updated.AllowedGroupIDs) != 2 {
		t.Fatalf("returned key channels = %v/%v", updated.GroupID, updated.AllowedGroupIDs)
	}

	// 换主通道：groupId=chanB，白名单含旧主通道
	gid2 := chanB
	updated, err = svc.UpdateSubKey(context.Background(), accountKey, 10, UpdateSubKeyRequest{
		GroupID:         &gid2,
		AllowedGroupIDs: &[]int64{mainOld},
	})
	if err != nil {
		t.Fatalf("UpdateSubKey(switch main): %v", err)
	}
	if keyRepo.gotGroupID == nil || *keyRepo.gotGroupID != chanB {
		t.Fatalf("main group write = %v, want %d", keyRepo.gotGroupID, chanB)
	}
	if len(keyRepo.gotAllowed) != 1 || keyRepo.gotAllowed[0] != mainOld {
		t.Fatalf("allowed write = %v, want [%d]", keyRepo.gotAllowed, mainOld)
	}
	if updated.GroupID == nil || *updated.GroupID != chanB {
		t.Fatalf("returned main = %v, want %d", updated.GroupID, chanB)
	}
}

func ptrInt64(v int64) *int64 { return &v }
