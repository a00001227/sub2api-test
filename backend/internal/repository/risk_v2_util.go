package repository

import "strconv"

// atoi64Safe 把字符串安全解析为 int64；解析失败返回 0。
// 原随 legacy v1 的 risk_sketch_cache.go 定义；v1 摘除后迁移至此供 Risk V2 仓储层复用。
func atoi64Safe(s string) int64 {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return n
}
