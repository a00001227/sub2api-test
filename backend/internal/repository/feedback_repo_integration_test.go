//go:build integration

package repository

import (
	"context"
	"fmt"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func mustCreateFeedback(t *testing.T, ctx context.Context, repo service.FeedbackRepository, userID int64, content string) *service.Feedback {
	t.Helper()
	f := &service.Feedback{UserID: userID, Type: "other", Content: content, Status: service.FeedbackStatusPending}
	require.NoError(t, repo.Create(ctx, f))
	return f
}

func TestFeedbackRepository_UnreadReplies(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	txCtx := dbent.NewTxContext(ctx, tx)
	client := tx.Client()

	user := mustCreateUser(t, client, &service.User{
		Email: fmt.Sprintf("feedback-unread-%d@example.com", time.Now().UnixNano()),
	})
	other := mustCreateUser(t, client, &service.User{
		Email: fmt.Sprintf("feedback-unread-other-%d@example.com", time.Now().UnixNano()),
	})
	repo := NewFeedbackRepository(client)

	replied := time.Now().Add(-time.Hour).Truncate(time.Microsecond)
	readAfter := replied.Add(10 * time.Minute)
	readBefore := replied.Add(-10 * time.Minute)
	reply := "已处理"

	// 1) 没有回复 → 不算未读
	mustCreateFeedback(t, txCtx, repo, user.ID, "no reply")
	// 2) 有回复、从未读 → 未读
	unread := mustCreateFeedback(t, txCtx, repo, user.ID, "unread")
	unread.AdminReply, unread.RepliedAt, unread.Status = &reply, &replied, service.FeedbackStatusResolved
	require.NoError(t, repo.Update(txCtx, unread))
	// 3) 有回复、已读(读在回复之后) → 已读
	seen := mustCreateFeedback(t, txCtx, repo, user.ID, "seen")
	seen.AdminReply, seen.RepliedAt, seen.ReplyReadAt, seen.Status = &reply, &replied, &readAfter, service.FeedbackStatusResolved
	require.NoError(t, repo.Update(txCtx, seen))
	// 4) 读过之后管理员又回复(replied_at 晚于 reply_read_at) → 重新未读
	reReplied := mustCreateFeedback(t, txCtx, repo, user.ID, "re-replied")
	reReplied.AdminReply, reReplied.RepliedAt, reReplied.ReplyReadAt, reReplied.Status = &reply, &replied, &readBefore, service.FeedbackStatusResolved
	require.NoError(t, repo.Update(txCtx, reReplied))
	// 5) 别的用户的未读回复 → 不计入
	foreign := mustCreateFeedback(t, txCtx, repo, other.ID, "foreign unread")
	foreign.AdminReply, foreign.RepliedAt, foreign.Status = &reply, &replied, service.FeedbackStatusResolved
	require.NoError(t, repo.Update(txCtx, foreign))

	n, err := repo.CountUnreadReplies(txCtx, user.ID)
	require.NoError(t, err)
	require.Equal(t, int64(2), n, "unread + re-replied")

	// 列表里 reply_read_at 原样带回,HasUnreadReply 与计数口径一致
	items, err := repo.ListByUser(txCtx, user.ID, 50)
	require.NoError(t, err)
	unreadInList := 0
	for i := range items {
		if items[i].HasUnreadReply() {
			unreadInList++
		}
	}
	require.Equal(t, 2, unreadInList)

	// 标记已读:只动当前用户的未读条目
	marked, err := repo.MarkRepliesRead(txCtx, user.ID, time.Now())
	require.NoError(t, err)
	require.Equal(t, int64(2), marked)

	n, err = repo.CountUnreadReplies(txCtx, user.ID)
	require.NoError(t, err)
	require.Zero(t, n)

	nOther, err := repo.CountUnreadReplies(txCtx, other.ID)
	require.NoError(t, err)
	require.Equal(t, int64(1), nOther, "别的用户不受影响")

	// 已读后管理员再次回复(走 service.Reply 同样的 Update 路径) → 又变未读
	got, err := repo.GetByID(txCtx, unread.ID)
	require.NoError(t, err)
	require.NotNil(t, got.ReplyReadAt)
	later := time.Now().Add(time.Minute)
	got.RepliedAt = &later
	require.NoError(t, repo.Update(txCtx, got))
	n, err = repo.CountUnreadReplies(txCtx, user.ID)
	require.NoError(t, err)
	require.Equal(t, int64(1), n)
}
