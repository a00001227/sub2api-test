package middleware

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// —— 最小 fakes（实现 service 侧导出接口）——

type mwEnfStore struct {
	counters   map[int64]int64
	modelCount map[string]int64
	rules      map[string]string
}

func (s *mwEnfStore) LoadAllowlist(ctx context.Context) ([]int64, error) { return nil, nil }
func (s *mwEnfStore) AddAllowlist(ctx context.Context, userID int64) error {
	return nil
}
func (s *mwEnfStore) RemoveAllowlist(ctx context.Context, userID int64) error { return nil }
func (s *mwEnfStore) IncrThrottleCounter(ctx context.Context, userID int64, ttl time.Duration) (int64, error) {
	if s.counters == nil {
		s.counters = map[int64]int64{}
	}
	s.counters[userID]++
	return s.counters[userID], nil
}
func (s *mwEnfStore) IncrModelThrottleCounter(ctx context.Context, userID int64, model string, ttl time.Duration) (int64, error) {
	if s.modelCount == nil {
		s.modelCount = map[string]int64{}
	}
	s.modelCount[model]++
	return s.modelCount[model], nil
}
func (s *mwEnfStore) LoadModelRules(ctx context.Context) (map[string]string, error) {
	return s.rules, nil
}
func (s *mwEnfStore) SetModelRule(ctx context.Context, model, action string) error {
	if s.rules == nil {
		s.rules = map[string]string{}
	}
	s.rules[model] = action
	return nil
}
func (s *mwEnfStore) DeleteModelRule(ctx context.Context, model string) error {
	delete(s.rules, model)
	return nil
}

type mwEnfRepo struct {
	items []service.RiskV2AssessmentListItem
}

func (r *mwEnfRepo) UpsertCurrentAssessment(ctx context.Context, userID int64, a service.RiskV2Assessment) (service.RiskV2UpsertResult, error) {
	return service.RiskV2Noop, nil
}
func (r *mwEnfRepo) GetCurrentAssessment(ctx context.Context, userID int64) (*service.RiskV2Assessment, bool, error) {
	return nil, false, nil
}
func (r *mwEnfRepo) ListCurrentAssessments(ctx context.Context, filter service.RiskV2ListFilter, page service.RiskV2Pagination) ([]service.RiskV2AssessmentListItem, error) {
	if page.Offset > 0 {
		return nil, nil
	}
	return r.items, nil
}
func (r *mwEnfRepo) DeleteByUserID(ctx context.Context, userID int64) error { return nil }
func (r *mwEnfRepo) CheckSchemaReady(ctx context.Context) error             { return nil }

func newActiveEnfSvc(rpm int) *service.EnforcementService {
	return newActiveEnfSvcRules(rpm, nil)
}

func newActiveEnfSvcRules(rpm int, rules map[string]string) *service.EnforcementService {
	store := &mwEnfStore{rules: rules}
	repo := &mwEnfRepo{items: []service.RiskV2AssessmentListItem{{UserID: 1, Confidence: 0.9, DataSufficient: true}}}
	return service.NewEnforcementService(store, repo, service.EnforcementConfigView{
		Enabled: true, ThrottleRPM: rpm, ConfidenceMin: 0.6, RefreshInterval: time.Minute, CounterTTL: time.Minute,
	})
}

func runEnf(svc *service.EnforcementService, prep func(c *gin.Context)) int {
	return runEnfBody(svc, "", prep)
}

func runEnfBody(svc *service.EnforcementService, body string, prep func(c *gin.Context)) int {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", reader)
	if prep != nil {
		prep(c)
	}
	nextCalled := false
	handlers := []gin.HandlerFunc{Enforcement(svc), func(c *gin.Context) { nextCalled = true; c.Status(http.StatusOK) }}
	for _, h := range handlers {
		if c.IsAborted() {
			break
		}
		h(c)
	}
	if nextCalled && w.Code == 0 {
		return http.StatusOK
	}
	return w.Code
}

func TestEnforcementMiddleware_ThrottlesHighUser(t *testing.T) {
	svc := newActiveEnfSvc(1)
	prep := func(c *gin.Context) { c.Set(string(ContextKeyAPIKey), &service.APIKey{UserID: 1}) }
	if code := runEnf(svc, prep); code != http.StatusOK {
		t.Fatalf("第 1 次应放行, got %d", code)
	}
	if code := runEnf(svc, prep); code != http.StatusTooManyRequests {
		t.Fatalf("第 2 次应 429, got %d", code)
	}
}

func TestEnforcementMiddleware_SkipsEdgeTrusted(t *testing.T) {
	svc := newActiveEnfSvc(1)
	prep := func(c *gin.Context) {
		c.Set(string(ContextKeyAPIKey), &service.APIKey{UserID: 1})
		c.Set(string(ContextKeyEdgeTrusted), true)
	}
	// 连发多次都应放行（cell 可信流量不限速）。
	for i := 0; i < 3; i++ {
		if code := runEnf(svc, prep); code != http.StatusOK {
			t.Fatalf("edge-trusted 应放行, got %d", code)
		}
	}
}

func TestEnforcementMiddleware_DisabledPassthrough(t *testing.T) {
	store := &mwEnfStore{}
	repo := &mwEnfRepo{items: []service.RiskV2AssessmentListItem{{UserID: 1, Confidence: 0.9, DataSufficient: true}}}
	svc := service.NewEnforcementService(store, repo, service.EnforcementConfigView{Enabled: false, ThrottleRPM: 1})
	prep := func(c *gin.Context) { c.Set(string(ContextKeyAPIKey), &service.APIKey{UserID: 1}) }
	for i := 0; i < 3; i++ {
		if code := runEnf(svc, prep); code != http.StatusOK {
			t.Fatalf("master 关应放行, got %d", code)
		}
	}
}

func TestEnforcementMiddleware_ModelBlocked(t *testing.T) {
	svc := newActiveEnfSvcRules(100, map[string]string{"claude-opus-4": "block"})
	prep := func(c *gin.Context) { c.Set(string(ContextKeyAPIKey), &service.APIKey{UserID: 1}) }
	// 受限模型 → 403（即便用户级 RPM 很高）。
	if code := runEnfBody(svc, `{"model":"claude-opus-4"}`, prep); code != http.StatusForbidden {
		t.Fatalf("受限模型应 403, got %d", code)
	}
	// 非受限模型 → 走用户级兜底（RPM=100 未超）→ 放行。
	if code := runEnfBody(svc, `{"model":"claude-sonnet-4"}`, prep); code != http.StatusOK {
		t.Fatalf("非受限模型应放行, got %d", code)
	}
}

func TestEnforcementMiddleware_ModelThrottle(t *testing.T) {
	svc := newActiveEnfSvcRules(1, map[string]string{"claude-opus-4": "throttle"})
	prep := func(c *gin.Context) { c.Set(string(ContextKeyAPIKey), &service.APIKey{UserID: 1}) }
	body := `{"model":"claude-opus-4"}`
	if code := runEnfBody(svc, body, prep); code != http.StatusOK {
		t.Fatalf("受限模型第 1 次应放行, got %d", code)
	}
	if code := runEnfBody(svc, body, prep); code != http.StatusTooManyRequests {
		t.Fatalf("受限模型第 2 次应 429, got %d", code)
	}
}
