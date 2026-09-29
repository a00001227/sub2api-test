package repository

import (
	"context"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

// claudeCLIVersionKey 缓存从 npm 拉到的 Claude Code 最新版本,进程重启后先用它,再等下一轮刷新。
const claudeCLIVersionKey = "claude_cli:latest_version"

type claudeCLIVersionCache struct {
	rdb *redis.Client
}

// NewClaudeCLIVersionCache 构造伪装 CLI 版本的 Redis 缓存。
func NewClaudeCLIVersionCache(rdb *redis.Client) service.ClaudeCLIVersionCache {
	return &claudeCLIVersionCache{rdb: rdb}
}

func (c *claudeCLIVersionCache) Get(ctx context.Context) (string, error) {
	if c == nil || c.rdb == nil {
		return "", nil
	}
	v, err := c.rdb.Get(ctx, claudeCLIVersionKey).Result()
	if errors.Is(err, redis.Nil) {
		return "", nil
	}
	return v, err
}

func (c *claudeCLIVersionCache) Set(ctx context.Context, version string, ttl time.Duration) error {
	if c == nil || c.rdb == nil {
		return nil
	}
	return c.rdb.Set(ctx, claudeCLIVersionKey, version, ttl).Err()
}
