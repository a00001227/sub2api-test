package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseAndWriteEnvRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ".env.cell1")
	content := "# comment\n\nPOSTGRES_PASSWORD=\"s3cr3t\"\nCELL_PORT=8091\nCELL_REGION=BOM\nEMPTY=\n"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	kv, err := parseEnvFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if kv["POSTGRES_PASSWORD"] != "s3cr3t" {
		t.Fatalf("quote strip failed: %q", kv["POSTGRES_PASSWORD"])
	}
	if kv["CELL_PORT"] != "8091" || kv["CELL_REGION"] != "BOM" {
		t.Fatalf("bad parse: %+v", kv)
	}
	if _, ok := kv["EMPTY"]; !ok {
		t.Fatalf("empty value key dropped")
	}

	out := filepath.Join(dir, ".env.cell2")
	if err := writeEnvFile(out, kv); err != nil {
		t.Fatal(err)
	}
	back, err := parseEnvFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if back["POSTGRES_PASSWORD"] != "s3cr3t" || back["CELL_REGION"] != "BOM" {
		t.Fatalf("round-trip mismatch: %+v", back)
	}
	// 0600 perms (env holds secrets).
	st, _ := os.Stat(out)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("want 0600, got %v", st.Mode().Perm())
	}
}

func TestSecretsTemplateStripsPerCellKeys(t *testing.T) {
	dir := t.TempDir()
	// An existing per-cell env with a mix of shared secrets + per-cell keys.
	content := "POSTGRES_PASSWORD=shared\nJWT_SECRET=jjj\nADMIN_PASSWORD=adminpw\n" +
		"CELL_PORT=8091\nCELL_REGION=BOM\nCELL_ADVERTISE_ADDR=http://x:8091\nCELL_MULTI_EGRESS=1\n"
	if err := os.WriteFile(filepath.Join(dir, ".env.cell3"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &Agent{cfg: &AgentConfig{DeployDir: dir}}
	kv, err := a.secretsTemplate()
	if err != nil {
		t.Fatal(err)
	}
	// Shared secrets (incl. ADMIN_PASSWORD) carry over.
	for _, k := range []string{"POSTGRES_PASSWORD", "JWT_SECRET", "ADMIN_PASSWORD"} {
		if _, ok := kv[k]; !ok {
			t.Fatalf("shared secret %s should carry over", k)
		}
	}
	// Per-cell keys are stripped (the agent re-sets these).
	for _, k := range []string{"CELL_PORT", "CELL_REGION", "CELL_ADVERTISE_ADDR", "CELL_MULTI_EGRESS"} {
		if _, ok := kv[k]; ok {
			t.Fatalf("per-cell key %s should be stripped", k)
		}
	}
}
