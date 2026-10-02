package main

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReportBootstrapAdminWritesFileNotLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bootstrap-admin-password")

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	const secret = "s3cr3t-generated-password"
	if err := reportBootstrapAdmin(path, "admin", true, secret); err != nil {
		t.Fatalf("reportBootstrapAdmin: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read password file: %v", err)
	}
	if strings.TrimSpace(string(data)) != secret {
		t.Errorf("file content = %q, want %q", data, secret)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %v, want 0600 (err=%v)", fi.Mode(), err)
	}
	if strings.Contains(buf.String(), secret) {
		t.Error("the generated password must never reach the logger, but it appears in log output")
	}
}

func TestReportBootstrapAdminNoOpWhenNotGenerated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bootstrap-admin-password")
	if err := reportBootstrapAdmin(path, "admin", false, ""); err != nil {
		t.Fatalf("reportBootstrapAdmin: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("must not write a file when no password was generated")
	}

	if err := reportBootstrapAdmin(path, "admin", true, ""); err != nil {
		t.Fatalf("reportBootstrapAdmin: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("must not write a file when generated is empty (admin password was pre-supplied)")
	}
}
