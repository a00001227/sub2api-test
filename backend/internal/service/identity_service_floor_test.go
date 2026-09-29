package service

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/stretchr/testify/require"
)

func TestFloorClaudeCLIVersion(t *testing.T) {
	cur := claude.CLICurrentVersion
	cases := []struct {
		name       string
		in         string
		want       string
		wantBumped bool
	}{
		{"older version floored", "claude-cli/2.1.278 (external, cli)", "claude-cli/" + cur + " (external, cli)", true},
		{"much older floored", "claude-cli/1.0.0 (external, cli)", "claude-cli/" + cur + " (external, cli)", true},
		{"case-insensitive product", "Claude-CLI/2.1.278 (external, cli)", "Claude-CLI/" + cur + " (external, cli)", true},
		{"current version untouched", "claude-cli/" + cur + " (external, cli)", "claude-cli/" + cur + " (external, cli)", false},
		{"newer version untouched", "claude-cli/9.9.9 (external, cli)", "claude-cli/9.9.9 (external, cli)", false},
		{"other product untouched", "python-httpx/0.27.0", "python-httpx/0.27.0", false},
		{"empty untouched", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, bumped := floorClaudeCLIVersion(tc.in)
			require.Equal(t, tc.want, got)
			require.Equal(t, tc.wantBumped, bumped)
		})
	}
}
