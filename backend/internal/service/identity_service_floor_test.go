package service

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/stretchr/testify/require"
)

func TestFloorClaudeCLIVersion(t *testing.T) {
	cur := claude.CurrentCLIVersion()
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

func TestNormalizeClaudeCLIFingerprint(t *testing.T) {
	cur := claude.CurrentCLIVersion()

	t.Run("non claude-cli UA replaced wholesale, ClientID kept", func(t *testing.T) {
		fp := &Fingerprint{
			ClientID:                "keep-me",
			UserAgent:               "Anthropic/Go 1.41.0",
			StainlessLang:           "go",
			StainlessPackageVersion: "1.41.0",
			StainlessOS:             "Linux",
			StainlessArch:           "x64",
			StainlessRuntime:        "go",
			StainlessRuntimeVersion: "go1.25.11",
		}
		require.True(t, normalizeClaudeCLIFingerprint(fp))
		require.Equal(t, "keep-me", fp.ClientID)
		require.Equal(t, claude.DefaultUserAgent(), fp.UserAgent)
		require.Equal(t, defaultFingerprint.StainlessLang, fp.StainlessLang)
		require.Equal(t, defaultFingerprint.StainlessPackageVersion, fp.StainlessPackageVersion)
		require.Equal(t, defaultFingerprint.StainlessOS, fp.StainlessOS)
		require.Equal(t, defaultFingerprint.StainlessArch, fp.StainlessArch)
		require.Equal(t, defaultFingerprint.StainlessRuntime, fp.StainlessRuntime)
		require.Equal(t, defaultFingerprint.StainlessRuntimeVersion, fp.StainlessRuntimeVersion)
		require.Contains(t, fp.UserAgent, "claude-cli/"+cur)
	})

	t.Run("empty UA replaced", func(t *testing.T) {
		fp := &Fingerprint{ClientID: "c"}
		require.True(t, normalizeClaudeCLIFingerprint(fp))
		require.Equal(t, claude.DefaultUserAgent(), fp.UserAgent)
	})

	t.Run("old claude-cli floored, stainless untouched", func(t *testing.T) {
		fp := &Fingerprint{UserAgent: "claude-cli/2.1.226 (external, cli)", StainlessOS: "Darwin"}
		require.True(t, normalizeClaudeCLIFingerprint(fp))
		require.Equal(t, "claude-cli/"+cur+" (external, cli)", fp.UserAgent)
		require.Equal(t, "Darwin", fp.StainlessOS)
	})

	t.Run("current or newer claude-cli untouched", func(t *testing.T) {
		for _, ua := range []string{"claude-cli/" + cur + " (external, cli)", "claude-cli/2.5.0 (external, cli)"} {
			fp := &Fingerprint{UserAgent: ua}
			require.False(t, normalizeClaudeCLIFingerprint(fp))
			require.Equal(t, ua, fp.UserAgent)
		}
	})

	t.Run("nil safe", func(t *testing.T) {
		require.False(t, normalizeClaudeCLIFingerprint(nil))
	})
}
