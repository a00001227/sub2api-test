package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Phase 21E-6C-2A: provider connect schema foundation must be forward-only,
// idempotent, and leave every existing row untouched (all new columns
// nullable, no backfill, no modification of existing columns/indexes).
func TestMigration160IsForwardOnlyAndIdempotent(t *testing.T) {
	content, err := FS.ReadFile("160_provider_connect_schema_foundation.sql")
	require.NoError(t, err)
	sql := string(content)

	// nullable additive columns only — no forced defaults on EXISTING
	// tables (the new provider_connect_sessions table may use defaults).
	require.Contains(t, sql, "ALTER TABLE proxies ADD COLUMN IF NOT EXISTS region VARCHAR(20)")
	require.Contains(t, sql,
		"ALTER TABLE accounts ADD COLUMN IF NOT EXISTS external_provider_account_id VARCHAR(64)")
	for _, line := range strings.Split(sql, "\n") {
		if strings.Contains(line, "ALTER TABLE") {
			require.NotContains(t, line, "NOT NULL",
				"ALTER on existing tables must stay nullable: %s", line)
			require.NotContains(t, line, "DEFAULT",
				"ALTER on existing tables must not force defaults: %s", line)
		}
	}

	// partial unique: uniqueness only where the reference is present
	require.Contains(t, sql, "CREATE UNIQUE INDEX IF NOT EXISTS uq_accounts_external_provider_account_id")
	require.Contains(t, sql, "WHERE external_provider_account_id IS NOT NULL")

	// new table with the full status lifecycle columns
	require.Contains(t, sql, "CREATE TABLE IF NOT EXISTS provider_connect_sessions")
	for _, col := range []string{
		"external_provider_account_id VARCHAR(64) NOT NULL",
		"provider_type VARCHAR(32) NOT NULL",
		"region VARCHAR(20)",
		"proxy_id BIGINT",
		"status VARCHAR(20) NOT NULL DEFAULT 'pending'",
		"oauth_session_id VARCHAR(128)",
		"callback_url VARCHAR(512) NOT NULL",
		"expires_at TIMESTAMPTZ NOT NULL",
		"completed_at TIMESTAMPTZ",
	} {
		require.Contains(t, sql, col)
	}

	// forward-only guarantees: no destructive or mutating statements
	for _, forbidden := range []string{"DROP TABLE", "DROP COLUMN", "ALTER COLUMN", "UPDATE ", "DELETE FROM"} {
		require.NotContains(t, sql, forbidden)
	}
	// no backfill of existing rows
	require.False(t, strings.Contains(sql, "INSERT INTO accounts") ||
		strings.Contains(sql, "INSERT INTO proxies"),
		"migration must not backfill existing tables")
}

// Phase 21E-6C-2C: session→account 链接列必须前向、幂等、可空。
func TestMigration161IsForwardOnlyAndIdempotent(t *testing.T) {
	content, err := FS.ReadFile("161_provider_connect_session_account_link.sql")
	require.NoError(t, err)
	sql := string(content)
	require.Contains(t, sql, "ADD COLUMN IF NOT EXISTS sub2api_account_id BIGINT")
	for _, forbidden := range []string{"NOT NULL", "DEFAULT", "DROP", "ALTER COLUMN", "UPDATE ", "DELETE FROM"} {
		require.NotContains(t, sql, forbidden)
	}
}
