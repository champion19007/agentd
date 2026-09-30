package main

import (
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"time"
)

type Component struct {
	Type        string `json:"type"`
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
	PURL        string `json:"purl,omitempty"`
	Hashes      []Hash `json:"hashes,omitempty"`
}

type Hash struct {
	Algorithm string `json:"alg"`
	Content   string `json:"content"`
}

type Metadata struct {
	Timestamp string    `json:"timestamp"`
	Tool      string    `json:"tool"`
	Component Component `json:"component"`
}

type CycloneDXSBOM struct {
	BOMFormat    string      `json:"bomFormat"`
	SpecVersion  string      `json:"specVersion"`
	SerialNumber string      `json:"serialNumber"`
	Version      int         `json:"version"`
	Metadata     Metadata    `json:"metadata"`
	Components   []Component `json:"components"`
}

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintf(os.Stderr, "Usage: %s <binary-path> <output-json-path>\n", os.Args[0])
		os.Exit(1)
	}

	binPath := os.Args[1]
	outPath := os.Args[2]

	info, err := buildinfo.ReadFile(binPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading buildinfo from %s: %v\n", binPath, err)
		os.Exit(1)
	}

	binFile, err := os.Open(binPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening binary %s: %v\n", binPath, err)
		os.Exit(1)
	}
	defer binFile.Close()

	hasher := sha256.New()
	if _, err := io.Copy(hasher, binFile); err != nil {
		fmt.Fprintf(os.Stderr, "Error calculating binary hash: %v\n", err)
		os.Exit(1)
	}
	binSHA256 := hex.EncodeToString(hasher.Sum(nil))

	rootPkg := info.Main.Path
	if rootPkg == "" {
		rootPkg = "github.com/champion19007/agentd"
	}
	rootVer := info.Main.Version
	if rootVer == "" || rootVer == "(devel)" {
		rootVer = "v1.0.0"
	}

	sbom := CycloneDXSBOM{
		BOMFormat:    "CycloneDX",
		SpecVersion:  "1.5",
		SerialNumber: fmt.Sprintf("urn:uuid:agentd-%s", binSHA256[:16]),
		Version:      1,
		Metadata: Metadata{
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Tool:      fmt.Sprintf("go-sbom-generator (%s)", runtime.Version()),
			Component: Component{
				Type:    "application",
				Name:    "agentd",
				Version: rootVer,
				PURL:    fmt.Sprintf("pkg:golang/%s@%s", rootPkg, rootVer),
				Hashes: []Hash{
					{Algorithm: "SHA-256", Content: binSHA256},
				},
			},
		},
		Components: make([]Component, 0, len(info.Deps)),
	}

	for _, dep := range info.Deps {
		comp := Component{
			Type:    "library",
			Name:    dep.Path,
			Version: dep.Version,
			PURL:    fmt.Sprintf("pkg:golang/%s@%s", dep.Path, dep.Version),
		}
		if dep.Sum != "" {
			comp.Hashes = append(comp.Hashes, Hash{
				Algorithm: "Go-Mod-Sum",
				Content:   dep.Sum,
			})
		}
		sbom.Components = append(sbom.Components, comp)
	}

	outData, err := json.MarshalIndent(sbom, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error marshaling SBOM: %v\n", err)
		os.Exit(1)
	}

	if err := os.WriteFile(outPath, outData, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "Error writing SBOM to %s: %v\n", outPath, err)
		os.Exit(1)
	}

	fmt.Printf("Generated CycloneDX SBOM at %s (%d components)\n", outPath, len(sbom.Components))
}
