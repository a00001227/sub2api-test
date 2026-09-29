package claude

import "testing"

func TestCompareCLIVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"2.1.280", "2.1.280", 0},
		{"2.1.281", "2.1.280", 1},
		{"2.1.279", "2.1.280", -1},
		{"2.2.0", "2.1.999", 1},
		{"3.0.0", "2.9.9", 1},
		{"2.1", "2.1.0", 0},
	}
	for _, tc := range cases {
		if got := CompareCLIVersions(tc.a, tc.b); got != tc.want {
			t.Errorf("Compare(%q,%q)=%d want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestSetCurrentCLIVersion(t *testing.T) {
	t.Cleanup(ResetCurrentCLIVersionForTest)
	ResetCurrentCLIVersionForTest()

	if CurrentCLIVersion() != CLICurrentVersion {
		t.Fatalf("default should be the constant, got %q", CurrentCLIVersion())
	}
	// 低于常量:拒绝,常量是下限
	if SetCurrentCLIVersion("2.1.100") {
		t.Fatal("lower than constant must be rejected")
	}
	// 预发布 / 非法格式:拒绝
	for _, bad := range []string{"2.1.300-beta.1", "v2.1.300", "2.1", "", "latest"} {
		if SetCurrentCLIVersion(bad) {
			t.Fatalf("%q must be rejected", bad)
		}
	}
	if CurrentCLIVersion() != CLICurrentVersion {
		t.Fatalf("rejected values must not change current, got %q", CurrentCLIVersion())
	}
	// 等于常量:接受
	if !SetCurrentCLIVersion(CLICurrentVersion) {
		t.Fatal("equal to constant must be accepted")
	}
	// 高于常量:接受并生效到 UA / DefaultHeaders
	if !SetCurrentCLIVersion(" 2.1.300 ") {
		t.Fatal("newer must be accepted")
	}
	if CurrentCLIVersion() != "2.1.300" {
		t.Fatalf("got %q", CurrentCLIVersion())
	}
	if DefaultUserAgent() != "claude-cli/2.1.300 (external, cli)" {
		t.Fatalf("UA not following runtime version: %q", DefaultUserAgent())
	}
	if DefaultHeaders()["User-Agent"] != DefaultUserAgent() {
		t.Fatal("DefaultHeaders must carry the runtime UA")
	}
	// DefaultHeaders 每次返回新 map,调用方改动不会污染模板
	h := DefaultHeaders()
	h["X-Stainless-Lang"] = "mutated"
	if DefaultHeaders()["X-Stainless-Lang"] == "mutated" {
		t.Fatal("DefaultHeaders must return a fresh copy")
	}
}
