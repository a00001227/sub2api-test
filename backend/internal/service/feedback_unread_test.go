package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFeedbackHasUnreadReply(t *testing.T) {
	replied := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	before := replied.Add(-time.Minute)
	after := replied.Add(time.Minute)

	cases := []struct {
		name string
		f    *Feedback
		want bool
	}{
		{"nil feedback", nil, false},
		{"no reply yet", &Feedback{}, false},
		{"replied, never read", &Feedback{RepliedAt: &replied}, true},
		{"replied, read afterwards", &Feedback{RepliedAt: &replied, ReplyReadAt: &after}, false},
		{"replied, read at same instant", &Feedback{RepliedAt: &replied, ReplyReadAt: &replied}, false},
		// 管理员再次回复:replied_at 刷新到已读时间之后 → 重新未读
		{"re-replied after read", &Feedback{RepliedAt: &replied, ReplyReadAt: &before}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, tc.f.HasUnreadReply())
		})
	}
}
