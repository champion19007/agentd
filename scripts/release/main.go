package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type Target struct {
	GOOS   string
	GOARCH string
}

var targets = []Target{
	{GOOS: "linux", GOARCH: "amd64"},
	{GOOS: "linux", GOARCH: "arm64"},
	{GOOS: "darwin", GOARCH: "amd64"},
	{GOOS: "darwin", GOARCH: "arm64"},
	{GOOS: "windows", GOARCH: "amd64"},
	{GOOS: "windows", GOARCH: "arm64"},
}

func main() {
	version := os.Getenv("AGENTD_VERSION")
	if version == "" {
		version = "v1.0.0"
	}

	commit := getGitCommit()
	buildDate := time.Now().UTC().Format(time.RFC3339)

	distDir := "dist"
	if err := os.RemoveAll(distDir); err != nil {
		fmt.Fprintf(os.Stderr, "Error cleaning %s: %v\n", distDir, err)
		os.Exit(1)
	}
	if err := os.MkdirAll(distDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "Error creating %s: %v\n", distDir, err)
		os.Exit(1)
	}

	fmt.Printf("=== Building Agentd Release %s (commit: %s, date: %s) ===\n", version, commit, buildDate)

	ldflags := fmt.Sprintf("-s -w -X github.com/champion19007/agentd/internal/cli.Version=%s -X github.com/champion19007/agentd/internal/cli.Commit=%s -X github.com/champion19007/agentd/internal/cli.Date=%s",
		version, commit, buildDate)

	var builtArchives []string
	var checksumEntries []string

	var sampleBinPath string

	for _, t := range targets {
		binName := "agentd"
		if t.GOOS == "windows" {
			binName = "agentd.exe"
		}
		targetBinPath := filepath.Join(distDir, fmt.Sprintf("agentd_%s_%s_%s", version, t.GOOS, t.GOARCH), binName)
		if err := os.MkdirAll(filepath.Dir(targetBinPath), 0755); err != nil {
			fmt.Fprintf(os.Stderr, "Error creating dir: %v\n", err)
			os.Exit(1)
		}

		fmt.Printf("--> Building %s/%s...\n", t.GOOS, t.GOARCH)
		cmd := exec.Command("go", "build", "-trimpath", "-ldflags", ldflags, "-o", targetBinPath, "./cmd/agentd")
		cmd.Env = append(os.Environ(),
			"CGO_ENABLED=0",
			"GOOS="+t.GOOS,
			"GOARCH="+t.GOARCH,
		)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr

		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "Failed building for %s/%s: %v\n", t.GOOS, t.GOARCH, err)
			os.Exit(1)
		}

		// Save a sample binary to run SBOM against
		if sampleBinPath == "" && t.GOOS == runtime.GOOS && t.GOARCH == runtime.GOARCH {
			sampleBinPath = targetBinPath
		}

		// Calculate binary checksum and size
		binSum, binSize, err := hashFile(targetBinPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error hashing %s: %v\n", targetBinPath, err)
			os.Exit(1)
		}
		fmt.Printf("    Built %s (%s, SHA256: %s...)\n", targetBinPath, formatBytes(binSize), binSum[:12])

		// Package into tar.gz or zip
		archiveName := fmt.Sprintf("agentd_%s_%s_%s", version, t.GOOS, t.GOARCH)
		var archivePath string
		if t.GOOS == "windows" {
			archivePath = filepath.Join(distDir, archiveName+".zip")
			batContent := []byte("@echo off\r\ntitle Agentd Web Dashboard\r\necho ======================================================\r\necho    Agentd - Autonomous Observation & Repair Daemon\r\necho ======================================================\r\necho.\r\necho Initializing local database if not already present...\r\n\"%~dp0agentd.exe\" init\r\necho.\r\necho Launching Web Dashboard in your default browser...\r\nstart http://127.0.0.1:8080/\r\necho.\r\necho Starting Agentd background server on http://127.0.0.1:8080/\r\necho (Keep this window open while using the dashboard)\r\necho Press Ctrl+C to stop the server.\r\necho.\r\n\"%~dp0agentd.exe\" serve\r\npause\r\n")
			extras := map[string][]byte{"start-dashboard.bat": batContent}
			if err := createZip(archivePath, targetBinPath, binName, extras); err != nil {
				fmt.Fprintf(os.Stderr, "Error creating zip: %v\n", err)
				os.Exit(1)
			}
		} else {
			archivePath = filepath.Join(distDir, archiveName+".tar.gz")
			if err := createTarGz(archivePath, targetBinPath, binName); err != nil {
				fmt.Fprintf(os.Stderr, "Error creating tar.gz: %v\n", err)
				os.Exit(1)
			}
		}

		arcSum, arcSize, err := hashFile(archivePath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error hashing archive: %v\n", err)
			os.Exit(1)
		}
		checksumEntries = append(checksumEntries, fmt.Sprintf("%s  %s", arcSum, filepath.Base(archivePath)))
		builtArchives = append(builtArchives, archivePath)
		fmt.Printf("    Archived %s (%s, SHA256: %s...)\n", filepath.Base(archivePath), formatBytes(arcSize), arcSum[:12])
	}

	// Write SHA256SUMS
	checksumPath := filepath.Join(distDir, "SHA256SUMS")
	checksumContent := strings.Join(checksumEntries, "\n") + "\n"
	if err := os.WriteFile(checksumPath, []byte(checksumContent), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "Error writing %s: %v\n", checksumPath, err)
		os.Exit(1)
	}
	fmt.Printf("--> Created %s with %d entries\n", checksumPath, len(checksumEntries))

	// Generate SBOM
	if sampleBinPath == "" && len(targets) > 0 {
		sampleBinPath = filepath.Join(distDir, fmt.Sprintf("agentd_%s_%s_%s", version, targets[0].GOOS, targets[0].GOARCH), "agentd")
	}
	sbomPath := filepath.Join(distDir, fmt.Sprintf("agentd_%s_sbom.json", version))
	fmt.Printf("--> Generating CycloneDX SBOM at %s...\n", sbomPath)
	sbomCmd := exec.Command("go", "run", "scripts/sbom/main.go", sampleBinPath, sbomPath)
	sbomCmd.Stdout = os.Stdout
	sbomCmd.Stderr = os.Stderr
	if err := sbomCmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "SBOM generation failed: %v\n", err)
		os.Exit(1)
	}

	// Run signing hook if available
	fmt.Printf("--> Invoking release signing hook...\n")
	if runtime.GOOS != "windows" {
		signCmd := exec.Command("/bin/sh", "scripts/sign.sh", checksumPath)
		signCmd.Stdout = os.Stdout
		signCmd.Stderr = os.Stderr
		_ = signCmd.Run()
	} else {
		fmt.Println("    (Windows host: signing hook script scripts/sign.sh ready for POSIX runner)")
	}

	fmt.Printf("\n=== Release Build Complete ===\n")
	fmt.Printf("Artifacts in %s:\n", distDir)
	entries, _ := os.ReadDir(distDir)
	for _, e := range entries {
		info, _ := e.Info()
		if !e.IsDir() {
			fmt.Printf("  %s (%s)\n", e.Name(), formatBytes(info.Size()))
		}
	}
}

func getGitCommit() string {
	cmd := exec.Command("git", "rev-parse", "--short", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()

	hasher := sha256.New()
	size, err := io.Copy(hasher, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hasher.Sum(nil)), size, nil
}

func createTarGz(tarPath, srcPath, internalName string) error {
	tarFile, err := os.Create(tarPath)
	if err != nil {
		return err
	}
	defer tarFile.Close()

	gw := gzip.NewWriter(tarFile)
	defer gw.Close()

	tw := tar.NewWriter(gw)
	defer tw.Close()

	srcFile, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	fi, err := srcFile.Stat()
	if err != nil {
		return err
	}

	hdr := &tar.Header{
		Name:    internalName,
		Size:    fi.Size(),
		Mode:    0755,
		ModTime: fi.ModTime(),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	_, err = io.Copy(tw, srcFile)
	return err
}

func createZip(zipPath, srcPath, internalName string, extraFiles map[string][]byte) error {
	zipFile, err := os.Create(zipPath)
	if err != nil {
		return err
	}
	defer zipFile.Close()

	zw := zip.NewWriter(zipFile)
	defer zw.Close()

	srcFile, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	fi, err := srcFile.Stat()
	if err != nil {
		return err
	}

	w, err := zw.Create(internalName)
	if err != nil {
		return err
	}

	if _, err = io.Copy(w, srcFile); err != nil {
		return err
	}

	for name, content := range extraFiles {
		ew, err := zw.Create(name)
		if err != nil {
			return err
		}
		if _, err := ew.Write(content); err != nil {
			return err
		}
	}
	_ = fi
	return nil
}

func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}
