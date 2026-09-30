package tests

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/champion19007/agentd/internal/adapters/secrets"
	"github.com/champion19007/agentd/internal/core/domain"
	"github.com/champion19007/agentd/internal/store/sqlite"
)

func TestSecurity_FilesystemPermissionsAndRefusal(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	// 1. Directory creation with safe permissions (0700)
	sensitiveDir := filepath.Join(dir, "sensitive_agentd_dir")
	dbPath := filepath.Join(sensitiveDir, "agentd.db")

	st, err := sqlite.Open(ctx, sqlite.Options{Path: dbPath})
	if err != nil {
		t.Fatalf("sqlite.Open failed: %v", err)
	}
	st.Close()

	info, err := os.Stat(sensitiveDir)
	if err != nil {
		t.Fatalf("stat failed: %v", err)
	}
	if runtime.GOOS != "windows" {
		if info.Mode().Perm()&0077 != 0 {
			t.Errorf("expected directory permissions 0700, got %o", info.Mode().Perm())
		}
	}

	// 2. Refusal of world-readable secret files
	secretsDir := filepath.Join(dir, "secrets")
	if err := os.MkdirAll(secretsDir, 0700); err != nil {
		t.Fatalf("mkdir secrets failed: %v", err)
	}

	secretFile := filepath.Join(secretsDir, "insecure_token")
	if err := os.WriteFile(secretFile, []byte("super-secret-token"), 0644); err != nil {
		t.Fatalf("write secret failed: %v", err)
	}

	// On POSIX,chmod to ensure world-readable bit is set
	_ = os.Chmod(secretFile, 0644)

	resolver := &secrets.Dir{Path: secretsDir}
	_, err = resolver.Resolve(ctx, []domain.SecretRef{"insecure_token"})
	if runtime.GOOS != "windows" {
		if err == nil {
			t.Fatal("expected resolver to refuse world-readable secret file, but it succeeded")
		}
		fail, ok := err.(domain.Failure)
		if !ok || (fail.Code != "secret_file_insecure" && fail.Code != "insecure_secret_permissions") {
			t.Fatalf("expected insecure secret permissions failure, got %v", err)
		}
	}

	// Now set safe permissions 0600 on the secret file
	_ = os.Chmod(secretFile, 0600)
	bundle, err := resolver.Resolve(ctx, []domain.SecretRef{"insecure_token"})
	if err != nil {
		t.Fatalf("expected successful resolution with safe permissions: %v", err)
	}
	sec, ok := bundle.Get("insecure_token")
	if !ok || sec.Reveal() != "super-secret-token" {
		t.Fatalf("expected super-secret-token, got %v", sec)
	}

	// 3. Refusal of world-readable database files
	if runtime.GOOS != "windows" {
		insecureDB := filepath.Join(dir, "insecure.db")
		if err := os.WriteFile(insecureDB, []byte("fake sqlite database"), 0644); err != nil {
			t.Fatalf("write insecure db failed: %v", err)
		}
		_ = os.Chmod(insecureDB, 0644)

		_, err = sqlite.Open(ctx, sqlite.Options{Path: insecureDB})
		if err == nil {
			t.Fatal("expected sqlite.Open to refuse world-readable database file, but it succeeded")
		}
	}
}
