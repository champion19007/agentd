package tests

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func findShell() string {
	if runtime.GOOS == "windows" {
		candidates := []string{
			`C:\Program Files\Git\bin\sh.exe`,
			`C:\Program Files\Git\usr\bin\sh.exe`,
			`C:\Program Files (x86)\Git\bin\sh.exe`,
		}
		for _, c := range candidates {
			if _, err := os.Stat(c); err == nil {
				return c
			}
		}
	}
	if path, err := exec.LookPath("sh"); err == nil {
		return path
	}
	return ""
}

func TestInstallScript_StrictChecksumVerification(t *testing.T) {
	sh := findShell()
	if sh == "" {
		t.Skip("POSIX shell (sh) not found on host; skipping install script integration test")
	}

	installScriptPath, err := filepath.Abs("../install.sh")
	if err != nil {
		t.Fatalf("resolving install.sh: %v", err)
	}
	if _, err := os.Stat(installScriptPath); err != nil {
		t.Fatalf("install.sh not found: %v", err)
	}

	distDir, err := filepath.Abs("../dist")
	if err != nil {
		t.Fatalf("resolving dist: %v", err)
	}

	// 1. Test Checksum Tampering Rejection
	t.Run("RejectsTamperedChecksum", func(t *testing.T) {
		tempBinDir := t.TempDir()
		tamperedContent := "bad_hash_000000000000000000000000000000000000000000000000000000000000  agentd_v1.0.0_linux_amd64.tar.gz\n"

		// Mock server serving tampered SHA256SUMS
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "SHA256SUMS") {
				w.Write([]byte(tamperedContent))
				return
			}
			// Serve dummy archive
			w.Write([]byte("dummy archive content"))
		}))
		defer ts.Close()

		scriptPath := filepath.ToSlash(installScriptPath)
		binDir := filepath.ToSlash(tempBinDir)

		cmd := exec.Command(sh, scriptPath)
		cmd.Env = append(os.Environ(),
			"AGENTD_DIST_URL="+ts.URL,
			"AGENTD_VERSION=v1.0.0",
			"AGENTD_OS=linux",
			"AGENTD_ARCH=amd64",
			"BINDIR="+binDir,
		)

		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("expected install.sh to fail on tampered checksum, but it succeeded:\n%s", string(out))
		}

		outStr := string(out)
		if !strings.Contains(outStr, "SECURITY ERROR") && !strings.Contains(outStr, "Checksum verification failed") {
			t.Errorf("expected security checksum verification error message, got:\n%s", outStr)
		}

		// Ensure nothing was installed into target dir
		entries, _ := os.ReadDir(tempBinDir)
		for _, e := range entries {
			if e.Name() == "agentd" || e.Name() == "agentd.exe" {
				t.Fatalf("compromised binary was written to target directory despite checksum mismatch!")
			}
		}
	})

	// 2. Test Legitimate Checksum Verification on Real Artifact
	t.Run("VerifiesLegitimateArtifact", func(t *testing.T) {
		linuxArchive := filepath.Join(distDir, "agentd_v1.0.0_linux_amd64.tar.gz")
		if _, err := os.Stat(linuxArchive); err != nil {
			t.Skip("dist/agentd_v1.0.0_linux_amd64.tar.gz not built; run scripts/release.go first")
		}

		data, err := os.ReadFile(linuxArchive)
		if err != nil {
			t.Fatalf("reading archive: %v", err)
		}
		hasher := sha256.New()
		hasher.Write(data)
		realHash := hex.EncodeToString(hasher.Sum(nil))

		legitContent := fmt.Sprintf("%s  agentd_v1.0.0_linux_amd64.tar.gz\n", realHash)

		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "SHA256SUMS") {
				w.Write([]byte(legitContent))
				return
			}
			http.ServeFile(w, r, linuxArchive)
		}))
		defer ts.Close()

		tempBinDir := t.TempDir()
		scriptPath := filepath.ToSlash(installScriptPath)
		binDir := filepath.ToSlash(tempBinDir)

		cmd := exec.Command(sh, scriptPath)
		cmd.Env = append(os.Environ(),
			"AGENTD_DIST_URL="+ts.URL,
			"AGENTD_VERSION=v1.0.0",
			"AGENTD_OS=linux",
			"AGENTD_ARCH=amd64",
			"BINDIR="+binDir,
		)

		out, _ := cmd.CombinedOutput()
		outStr := string(out)

		if strings.Contains(outStr, "SECURITY ERROR") {
			t.Errorf("legitimate checksum was falsely rejected as security error:\n%s", outStr)
		}
		if !strings.Contains(outStr, "Checksum verified successfully") {
			t.Errorf("expected 'Checksum verified successfully', got output:\n%s", outStr)
		}
	})
}
