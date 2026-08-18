//go:build unit

package repository

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func i64toa(v int64) string { return strconv.FormatInt(v, 10) }

// —— Worker 单元测试用 fakes（放在 repository 包，因为 service 包测试二进制存在与本功能无关的既有编译失败）——

// fakeScoringReader 实现 service.RiskV2ScoringReader。
type fakeScoringReader struct {
	mu sync.Mutex
	// cycleEnd -> 有序 user 列表
	cycleUsers map[int64][]int64
	// user -> snapshot
	snaps map[int64]service.RiskV2ScoringSnapshot
	// 注入：单用户读错误
	userErr map[int64]error
	// 注入：每次 batch/list 延迟（模拟 Redis RTT）
	delay time.Duration
	// 注入：list 错误
	listErr error
	// 统计
	batchCalls int64
	listCalls  int64
}

func newFakeReader() *fakeScoringReader {
	return &fakeScoringReader{
		cycleUsers: map[int64][]int64{},
		snaps:      map[int64]service.RiskV2ScoringSnapshot{},
		userErr:    map[int64]error{},
	}
}

func (f *fakeScoringReader) setCycle(cycleEnd int64, users []int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cycleUsers[cycleEnd] = append([]int64(nil), users...)
	for _, u := range users {
		if _, ok := f.snaps[u]; !ok {
			f.snaps[u] = service.RiskV2ScoringSnapshot{UserID: u, SchemaVersion: "s1", FingerprintKeyVersion: "v1"}
		}
	}
}

func (f *fakeScoringReader) ReadForScoring(ctx context.Context, userID int64) (service.RiskV2ScoringSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if e := f.userErr[userID]; e != nil {
		return service.RiskV2ScoringSnapshot{UserID: userID}, e
	}
	return f.snaps[userID], nil
}

func (f *fakeScoringReader) ReadForScoringBatch(ctx context.Context, userIDs []int64) ([]service.RiskV2ScoringBatchItem, error) {
	atomic.AddInt64(&f.batchCalls, 1)
	if f.delay > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(f.delay):
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]service.RiskV2ScoringBatchItem, len(userIDs))
	for i, u := range userIDs {
		out[i] = service.RiskV2ScoringBatchItem{UserID: u}
		if e := f.userErr[u]; e != nil {
			out[i].Err = e
			continue
		}
		out[i].Snapshot = f.snaps[u]
	}
	return out, nil
}

func (f *fakeScoringReader) ListActiveUserIDsForCycle(ctx context.Context, cycleEnd int64, cursor string, limit int) (service.RiskV2ActiveUserPage, error) {
	atomic.AddInt64(&f.listCalls, 1)
	if f.listErr != nil {
		return service.RiskV2ActiveUserPage{}, f.listErr
	}
	if err := ctx.Err(); err != nil {
		return service.RiskV2ActiveUserPage{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	users := f.cycleUsers[cycleEnd]
	// cursor = 上一页最后一个 user 的十进制字符串；从其后开始。
	start := 0
	if cursor != "" {
		for i, u := range users {
			if i64toa(u) == cursor {
				start = i + 1
				break
			}
		}
	}
	var page service.RiskV2ActiveUserPage
	end := start + limit
	if end > len(users) {
		end = len(users)
	}
	page.UserIDs = append(page.UserIDs, users[start:end]...)
	if end < len(users) && len(page.UserIDs) > 0 {
		page.NextCursor = i64toa(page.UserIDs[len(page.UserIDs)-1])
	}
	return page, nil
}

// fakeLease 实现 service.RiskV2Lease。
type fakeLease struct {
	mu            sync.Mutex
	held          bool
	holder        string
	acquireErr    error
	renewCallsMax int // >0：第 N 次 renew 后返回 lost
	renewCalls    int
	renewErr      error
	releases      int
	token         string
	acquireCount  int
	contendAlways bool
}

func (l *fakeLease) Acquire(ctx context.Context, ttl time.Duration) (string, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.acquireCount++
	if l.acquireErr != nil {
		return "", false, l.acquireErr
	}
	if l.contendAlways || l.held {
		return "", false, nil
	}
	l.held = true
	l.token = "tok-" + i64toa(int64(l.acquireCount))
	l.holder = l.token
	return l.token, true, nil
}

func (l *fakeLease) Renew(ctx context.Context, token string, ttl time.Duration) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.renewCalls++
	if l.renewErr != nil {
		return false, l.renewErr
	}
	if token != l.holder {
		return false, nil
	}
	if l.renewCallsMax > 0 && l.renewCalls >= l.renewCallsMax {
		l.held = false // 模拟 lease 丢失
		return false, nil
	}
	return true, nil
}

func (l *fakeLease) Release(ctx context.Context, token string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if token == l.holder {
		l.held = false
		l.releases++
	}
	return nil
}

// fakeHealth 实现 service.RiskV2IngestionHealthReader。
type fakeHealth struct {
	h   service.RiskV2IngestionHealth
	err error
}

func (f *fakeHealth) ReadIngestionHealth(ctx context.Context, cycleEnd int64, windowMinutes int) (service.RiskV2IngestionHealth, error) {
	return f.h, f.err
}

// fakeRepo 实现 service.UserRiskV2Repository（只用 UpsertCurrentAssessment；其余 no-op）。
type fakeRepo struct {
	mu         sync.Mutex
	upserts    []upsertRec
	result     service.RiskV2UpsertResult
	errByUser  map[int64]error
	defaultErr error
	schemaErr  error
}

type upsertRec struct {
	userID int64
	a      service.RiskV2Assessment
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{result: service.RiskV2Inserted, errByUser: map[int64]error{}}
}

func (r *fakeRepo) UpsertCurrentAssessment(ctx context.Context, userID int64, a service.RiskV2Assessment) (service.RiskV2UpsertResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e := r.errByUser[userID]; e != nil {
		return 0, e
	}
	if r.defaultErr != nil {
		return 0, r.defaultErr
	}
	r.upserts = append(r.upserts, upsertRec{userID: userID, a: a})
	return r.result, nil
}
func (r *fakeRepo) GetCurrentAssessment(ctx context.Context, userID int64) (*service.RiskV2Assessment, bool, error) {
	return nil, false, nil
}
func (r *fakeRepo) ListCurrentAssessments(ctx context.Context, f service.RiskV2ListFilter, p service.RiskV2Pagination) ([]service.RiskV2AssessmentListItem, error) {
	return nil, nil
}
func (r *fakeRepo) DeleteByUserID(ctx context.Context, userID int64) error { return nil }
func (r *fakeRepo) CheckSchemaReady(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.schemaErr
}

func (r *fakeRepo) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.upserts)
}

// fakeCycleStore 实现 service.RiskV2CycleStore（owner=明文 token 比较，简化测试）。
type fakeCycleStore struct {
	mu          sync.Mutex
	states      map[int64]service.RiskV2CycleState
	owners      map[int64]string                    // cycleEnd -> ownerToken
	retry       map[int64][]service.RiskV2RetryUser // 预置的待读 retry 用户
	added       map[int64][]service.RiskV2RetryUser // AddRetryUsers 记录：targetCycle -> users
	maxAttempts int
	overflow    bool
	failSave    bool // 模拟 owner 被接管 → SaveCycleState 返回 ok=false
}

func newFakeCycleStore() *fakeCycleStore {
	return &fakeCycleStore{
		states: map[int64]service.RiskV2CycleState{}, owners: map[int64]string{},
		retry: map[int64][]service.RiskV2RetryUser{}, added: map[int64][]service.RiskV2RetryUser{},
		maxAttempts: 3,
	}
}

func (s *fakeCycleStore) LoadCycleState(ctx context.Context, cycleEnd int64) (service.RiskV2CycleState, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.states[cycleEnd]
	return st, ok, nil
}
func (s *fakeCycleStore) ClaimCycleState(ctx context.Context, cycleEnd int64, ownerToken string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.owners[cycleEnd] = ownerToken // 接管：强制 owner
	st := s.states[cycleEnd]
	st.Status = service.RiskV2CycleRunning
	s.states[cycleEnd] = st
	return nil
}
func (s *fakeCycleStore) SaveCycleState(ctx context.Context, cycleEnd int64, st service.RiskV2CycleState, ownerToken string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failSave {
		return false, nil // 模拟 owner 被别的 leader 接管 → 陈旧写被拒
	}
	if cur, ok := s.owners[cycleEnd]; ok && cur != "" && cur != ownerToken {
		return false, nil // owner 不匹配
	}
	s.owners[cycleEnd] = ownerToken
	st.OwnerTokenHash = ownerToken
	s.states[cycleEnd] = st
	return true, nil
}
func (s *fakeCycleStore) AddRetryUsers(ctx context.Context, targetCycleEnd int64, users []service.RiskV2RetryUser) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, u := range users {
		if u.Attempts > s.maxAttempts {
			continue // 超过上限 → 丢弃（永久放弃）
		}
		s.added[targetCycleEnd] = append(s.added[targetCycleEnd], u)
	}
	return s.overflow, nil
}
func (s *fakeCycleStore) LoadRetryUsers(ctx context.Context, cycleEnd int64, limit int) ([]service.RiskV2RetryUser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.retry[cycleEnd], nil
}
func (s *fakeCycleStore) addedTo(cycleEnd int64) []service.RiskV2RetryUser {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.added[cycleEnd]
}

// —— 测试时钟：now 可手动推进；sleep 立即推进 now（不真正睡眠）——
type manualClock struct {
	mu sync.Mutex
	t  time.Time
}

func newManualClock(t time.Time) *manualClock { return &manualClock{t: t} }
func (c *manualClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *manualClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}
func (c *manualClock) sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.advance(d)
	return nil
}

// defaultWorkerParams 保守默认（对齐 config 默认，但用于测试更短的 lease renew）。
func defaultWorkerParams() service.RiskV2WorkerParams {
	return service.RiskV2WorkerParams{
		Interval:             5 * time.Minute,
		GraceDelay:           20 * time.Second,
		CycleTimeout:         240 * time.Second,
		BatchSize:            100,
		MaxUsersPerSecond:    50,
		MaxUsersPerCycle:     10000,
		MaxReadErrorRatio:    0.5,
		MaxDBErrorRatio:      0.5,
		LeaseTTL:             30 * time.Second,
		LeaseRenewInterval:   10 * time.Second,
		MaxCatchupCycles:     3,
		AssessmentStaleAfter: time.Hour,
	}
}

func healthyHealth() *fakeHealth {
	return &fakeHealth{h: service.RiskV2IngestionHealth{HealthAvailable: true, AggregationHealthy: true, ObservationDropRatioAvailable: true}}
}
