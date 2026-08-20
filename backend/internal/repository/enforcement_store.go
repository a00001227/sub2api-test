package repository

import (
	"context"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// 蒸馏执行层 Redis 存储：豁免名单 + 独立限速计数（与 riskv2:* / evcap:* 键完全隔离）。
// 键：
//
//	enf:allowlist            SET(userID)              —— 豁免名单，重启可恢复。
//	enf:rpm:<userID>:<min>   STRING(计数, EXPIRE)     —— 命中 HIGH 时的独立分钟桶，不动用户正常 RPM。
const (
	enfAllowlistKey   = "enf:allowlist"
	enfRPMPrefix      = "enf:rpm:"
	enfModelRulesKey  = "enf:modelrules" // HASH field=model value=action(block|throttle)
	enfModelRPMPrefix = "enf:mrpm:"      // 按 (user,model) 独立分钟桶
)

type enforcementStore struct{ rdb *redis.Client }

// NewEnforcementStore 构造执行层存储；rdb 为 nil → nil（服务侧据此禁用）。
func NewEnforcementStore(rdb *redis.Client) service.EnforcementStore {
	if rdb == nil {
		return nil
	}
	return &enforcementStore{rdb: rdb}
}

func (s *enforcementStore) LoadAllowlist(ctx context.Context) ([]int64, error) {
	members, err := s.rdb.SMembers(ctx, enfAllowlistKey).Result()
	if err != nil {
		return nil, err
	}
	out := make([]int64, 0, len(members))
	for _, m := range members {
		if id, err := strconv.ParseInt(m, 10, 64); err == nil && id > 0 {
			out = append(out, id)
		}
	}
	return out, nil
}

func (s *enforcementStore) AddAllowlist(ctx context.Context, userID int64) error {
	return s.rdb.SAdd(ctx, enfAllowlistKey, strconv.FormatInt(userID, 10)).Err()
}

func (s *enforcementStore) RemoveAllowlist(ctx context.Context, userID int64) error {
	return s.rdb.SRem(ctx, enfAllowlistKey, strconv.FormatInt(userID, 10)).Err()
}

// IncrThrottleCounter 对当前分钟桶自增并首次设置兜底 TTL；返回本分钟内累计计数。
func (s *enforcementStore) IncrThrottleCounter(ctx context.Context, userID int64, ttl time.Duration) (int64, error) {
	minute := time.Now().Unix() / 60
	key := enfRPMPrefix + strconv.FormatInt(userID, 10) + ":" + strconv.FormatInt(minute, 10)
	return s.incrBucket(ctx, key, ttl)
}

// IncrModelThrottleCounter 对某 (user,model) 的当前分钟桶自增；受限模型的独立限速计数。
func (s *enforcementStore) IncrModelThrottleCounter(ctx context.Context, userID int64, model string, ttl time.Duration) (int64, error) {
	minute := time.Now().Unix() / 60
	key := enfModelRPMPrefix + strconv.FormatInt(userID, 10) + ":" + model + ":" + strconv.FormatInt(minute, 10)
	return s.incrBucket(ctx, key, ttl)
}

func (s *enforcementStore) incrBucket(ctx context.Context, key string, ttl time.Duration) (int64, error) {
	pipe := s.rdb.TxPipeline()
	incr := pipe.Incr(ctx, key)
	if ttl <= 0 {
		ttl = 2 * time.Minute
	}
	pipe.Expire(ctx, key, ttl)
	if _, err := pipe.Exec(ctx); err != nil {
		return 0, err
	}
	return incr.Val(), nil
}

// LoadModelRules 载入受限模型规则（model → action）。
func (s *enforcementStore) LoadModelRules(ctx context.Context) (map[string]string, error) {
	return s.rdb.HGetAll(ctx, enfModelRulesKey).Result()
}

// SetModelRule 设置某受限模型的处置动作（block|throttle）。
func (s *enforcementStore) SetModelRule(ctx context.Context, model, action string) error {
	return s.rdb.HSet(ctx, enfModelRulesKey, model, action).Err()
}

// DeleteModelRule 移除某受限模型规则。
func (s *enforcementStore) DeleteModelRule(ctx context.Context, model string) error {
	return s.rdb.HDel(ctx, enfModelRulesKey, model).Err()
}
