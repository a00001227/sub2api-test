package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// stubFeedbackRepo 只实现未读计数/标记已读,其余方法不应被调用。
type stubFeedbackRepo struct {
	service.FeedbackRepository
	unread      int64
	countedUser int64
	markedUser  int64
	markedAt    time.Time
	getByIDHits int
}

func (s *stubFeedbackRepo) CountUnreadReplies(_ context.Context, userID int64) (int64, error) {
	s.countedUser = userID
	return s.unread, nil
}

func (s *stubFeedbackRepo) MarkRepliesRead(_ context.Context, userID int64, at time.Time) (int64, error) {
	s.markedUser = userID
	s.markedAt = at
	n := s.unread
	s.unread = 0
	return n, nil
}

func (s *stubFeedbackRepo) GetByID(_ context.Context, _ int64) (*service.Feedback, error) {
	s.getByIDHits++
	return nil, service.ErrFeedbackNotFound
}

func (s *stubFeedbackRepo) ListByUser(_ context.Context, _ int64, _ int) ([]service.Feedback, error) {
	return nil, nil
}

func (s *stubFeedbackRepo) List(_ context.Context, _ pagination.PaginationParams, _ service.FeedbackListFilters) ([]service.Feedback, *pagination.PaginationResult, error) {
	return nil, nil, nil
}

// newFeedbackTestRouter 按 routes/user.go 的顺序注册工单路由(静态段在 /:id 之前)。
func newFeedbackTestRouter(repo *stubFeedbackRepo, userID int64) *gin.Engine {
	gin.SetMode(gin.TestMode)
	h := NewFeedbackHandler(service.NewFeedbackService(repo, nil))
	r := gin.New()
	if userID > 0 {
		r.Use(func(c *gin.Context) {
			c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: userID})
		})
	}
	g := r.Group("/feedback")
	g.GET("", h.List)
	g.GET("/unread-count", h.UnreadCount)
	g.POST("/read", h.MarkRead)
	g.GET("/:id", h.GetByID)
	return r
}

func TestFeedbackHandler_UnreadCount(t *testing.T) {
	repo := &stubFeedbackRepo{unread: 3}
	r := newFeedbackTestRouter(repo, 77)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/feedback/unread-count", nil))

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var body struct {
		Data FeedbackUnreadCountResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Equal(t, int64(3), body.Data.Count)
	require.Equal(t, int64(77), repo.countedUser, "计数必须按当前登录用户")
	require.Zero(t, repo.getByIDHits, "静态路由 /unread-count 不能被 /:id 吞掉")
}

func TestFeedbackHandler_MarkRead(t *testing.T) {
	repo := &stubFeedbackRepo{unread: 2}
	r := newFeedbackTestRouter(repo, 77)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/feedback/read", nil))

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var body struct {
		Data FeedbackMarkReadResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Equal(t, int64(2), body.Data.Marked)
	require.Equal(t, int64(77), repo.markedUser)
	require.WithinDuration(t, time.Now(), repo.markedAt, 5*time.Second)

	// 标记后再查 → 0
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, httptest.NewRequest(http.MethodGet, "/feedback/unread-count", nil))
	require.Equal(t, http.StatusOK, w2.Code)
	require.Contains(t, w2.Body.String(), `"count":0`)
}

func TestFeedbackHandler_UnreadEndpointsRequireAuth(t *testing.T) {
	repo := &stubFeedbackRepo{unread: 1}
	r := newFeedbackTestRouter(repo, 0)

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/feedback/unread-count"},
		{http.MethodPost, "/feedback/read"},
	} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
		require.Equal(t, http.StatusUnauthorized, w.Code, "%s %s", tc.method, tc.path)
	}
	require.Zero(t, repo.countedUser)
	require.Zero(t, repo.markedUser)
}
