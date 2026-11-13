// Package igloader loads HL7 FHIR R4 Implementation Guide (IG) NPM packages
// from compressed .tgz / .tar.gz archives and unpacked directories.
//
// It implements the HL7 FHIR NPM Package Specification in pure Go with strict
// archive security bounds (path-traversal rejection, symlink/device rejection,
// entry count caps, and decompression bomb byte limits) and deterministic
// topological dependency ordering across multi-package installations.
package igloader

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

var (
	// ErrEmptyArchive is returned when a compressed IG stream is empty.
	ErrEmptyArchive = errors.New("igloader: archive payload is empty")
	// ErrArchiveTooLarge is returned when the compressed archive exceeds Limits.MaxArchiveBytes.
	ErrArchiveTooLarge = errors.New("igloader: compressed archive exceeds maximum size")
	// ErrDecompressedSizeExceeded is returned when cumulative unpacked bytes exceed Limits.MaxDecompressedBytes.
	ErrDecompressedSizeExceeded = errors.New("igloader: decompressed archive exceeds maximum size")
	// ErrFileTooLarge is returned when a single archive or directory file exceeds Limits.MaxSingleFileBytes.
	ErrFileTooLarge = errors.New("igloader: single file exceeds maximum size")
	// ErrTooManyEntries is returned when an archive or directory exceeds Limits.MaxEntries.
	ErrTooManyEntries = errors.New("igloader: package exceeds maximum entry count")
	// ErrUnsafeArchivePath is returned when an archive entry contains path traversal or absolute segments.
	ErrUnsafeArchivePath = errors.New("igloader: unsafe archive entry path")
	// ErrUnsafeTarEntry is returned when an archive contains symlinks, hardlinks, or special device files.
	ErrUnsafeTarEntry = errors.New("igloader: unsafe non-regular tar entry type")
	// ErrUnsafeSymlink is returned when an unpacked IG directory contains a symbolic link.
	ErrUnsafeSymlink = errors.New("igloader: symbolic links are not permitted inside IG directory")
	// ErrMissingManifest is returned when package/package.json (or package.json) is absent.
	ErrMissingManifest = errors.New("igloader: missing package.json manifest")
	// ErrInvalidManifest is returned when package.json is malformed or missing required fields.
	ErrInvalidManifest = errors.New("igloader: invalid package.json manifest")
	// ErrIncompatibleFHIRVersion is returned when a package does not support FHIR R4 (4.0.x).
	ErrIncompatibleFHIRVersion = errors.New("igloader: package FHIR version is incompatible with R4 (4.0.x)")
	// ErrDependencyCycle is returned when a set of IG packages has a circular dependency graph.
	ErrDependencyCycle = errors.New("igloader: circular package dependency detected")
)

// Limits bounds resource consumption when unpacking .tgz archives or scanning directories.
type Limits struct {
	MaxArchiveBytes      int64
	MaxDecompressedBytes int64
	MaxSingleFileBytes   int64
	MaxEntries           int
}

// DefaultLimits returns production-safe decompression and entry limits for FHIR NPM packages.
func DefaultLimits() Limits {
	return Limits{
		MaxArchiveBytes:      128 << 20, // 128 MiB compressed
		MaxDecompressedBytes: 256 << 20, // 256 MiB decompressed total
		MaxSingleFileBytes:   32 << 20,  // 32 MiB single JSON file
		MaxEntries:           10000,
	}
}

func normalizeLimits(l Limits) Limits {
	def := DefaultLimits()
	if l.MaxArchiveBytes <= 0 {
		l.MaxArchiveBytes = def.MaxArchiveBytes
	}
	if l.MaxDecompressedBytes <= 0 {
		l.MaxDecompressedBytes = def.MaxDecompressedBytes
	}
	if l.MaxSingleFileBytes <= 0 {
		l.MaxSingleFileBytes = def.MaxSingleFileBytes
	}
	if l.MaxEntries <= 0 {
		l.MaxEntries = def.MaxEntries
	}
	return l
}

// Options configures FHIR NPM package loading behavior.
type Options struct {
	// LoadExamples includes clinical example instances (such as package/examples/*.json
	// or non-conformance sample resources) alongside conformance artifacts.
	LoadExamples bool
	// Limits overrides default archive and directory size caps when positive.
	Limits Limits
}

// Manifest represents the parsed HL7 FHIR NPM package/package.json metadata.
type Manifest struct {
	Name         string            `json:"name"`
	Version      string            `json:"version"`
	FHIRVersions []string          `json:"fhirVersions,omitempty"`
	Canonical    string            `json:"canonical,omitempty"`
	URL          string            `json:"url,omitempty"`
	Title        string            `json:"title,omitempty"`
	Description  string            `json:"description,omitempty"`
	Type         string            `json:"type,omitempty"`
	Author       string            `json:"author,omitempty"`
	License      string            `json:"license,omitempty"`
	Dependencies map[string]string `json:"dependencies,omitempty"`
}

// Artifact represents a single FHIR R4 JSON resource extracted from an IG package.
type Artifact struct {
	Filename     string         `json:"filename"`
	ResourceType string         `json:"resourceType"`
	ID           string         `json:"id,omitempty"`
	URL          string         `json:"url,omitempty"`
	Version      string         `json:"version,omitempty"`
	Name         string         `json:"name,omitempty"`
	Status       string         `json:"status,omitempty"`
	Kind         string         `json:"kind,omitempty"`
	Type         string         `json:"type,omitempty"`
	Abstract     bool           `json:"abstract,omitempty"`
	IsExample    bool           `json:"isExample,omitempty"`
	RawJSON      []byte         `json:"-"`
	Parsed       map[string]any `json:"-"`
}

// Package represents a loaded and validated HL7 FHIR R4 NPM Implementation Guide package.
type Package struct {
	Manifest  Manifest   `json:"manifest"`
	Source    string     `json:"source,omitempty"`
	LoadedAt  time.Time  `json:"loadedAt"`
	Artifacts []Artifact `json:"artifacts"`
}

// PackageCoord returns the "name#version" coordinate of the package.
func (p *Package) PackageCoord() string {
	if p == nil {
		return ""
	}
	if p.Manifest.Version == "" {
		return p.Manifest.Name
	}
	return p.Manifest.Name + "#" + p.Manifest.Version
}

// CanonicalURI returns the package's canonical URL (falling back to ImplementationGuide.url if present).
func (p *Package) CanonicalURI() string {
	if p == nil {
		return ""
	}
	if c := strings.TrimSpace(p.Manifest.Canonical); c != "" {
		return c
	}
	if u := strings.TrimSpace(p.Manifest.URL); u != "" {
		return u
	}
	for _, a := range p.Artifacts {
		if a.ResourceType == "ImplementationGuide" && strings.TrimSpace(a.URL) != "" {
			return strings.TrimSpace(a.URL)
		}
	}
	return ""
}

// VersionedCanonical returns "canonical|version" when both canonical URL and version are present,
// or CanonicalURI() otherwise.
func (p *Package) VersionedCanonical() string {
	if p == nil {
		return ""
	}
	c := p.CanonicalURI()
	if c == "" {
		return ""
	}
	if strings.Contains(c, "|") || strings.TrimSpace(p.Manifest.Version) == "" {
		return c
	}
	return c + "|" + strings.TrimSpace(p.Manifest.Version)
}

// ConformanceArtifacts returns all non-example conformance artifacts in deterministic registration order.
func (p *Package) ConformanceArtifacts() []Artifact {
	if p == nil {
		return nil
	}
	out := make([]Artifact, 0, len(p.Artifacts))
	for _, a := range p.Artifacts {
		if !a.IsExample {
			out = append(out, a)
		}
	}
	return out
}

// ExampleArtifacts returns all example resources loaded when Options.LoadExamples is enabled.
func (p *Package) ExampleArtifacts() []Artifact {
	if p == nil {
		return nil
	}
	var out []Artifact
	for _, a := range p.Artifacts {
		if a.IsExample {
			out = append(out, a)
		}
	}
	return out
}

// ArtifactsByType returns all loaded artifacts matching resType (case-insensitive).
func (p *Package) ArtifactsByType(resType string) []Artifact {
	if p == nil {
		return nil
	}
	var out []Artifact
	for _, a := range p.Artifacts {
		if strings.EqualFold(a.ResourceType, resType) {
			out = append(out, a)
		}
	}
	return out
}

type rawFileEntry struct {
	cleanPath string
	data      []byte
}

// LoadTGZBytes parses and validates an HL7 FHIR NPM .tgz archive from an in-memory byte slice.
func LoadTGZBytes(raw []byte, opts Options) (*Package, error) {
	if len(raw) == 0 {
		return nil, ErrEmptyArchive
	}
	limits := normalizeLimits(opts.Limits)
	if int64(len(raw)) > limits.MaxArchiveBytes {
		return nil, fmt.Errorf("%w: got %d bytes, limit %d", ErrArchiveTooLarge, len(raw), limits.MaxArchiveBytes)
	}
	return LoadTGZReader(bytes.NewReader(raw), opts)
}

// LoadTGZFile reads, parses, and validates an HL7 FHIR NPM .tgz / .tar.gz file on disk.
func LoadTGZFile(filePath string, opts Options) (*Package, error) {
	limits := normalizeLimits(opts.Limits)
	info, err := os.Stat(filePath)
	if err != nil {
		return nil, fmt.Errorf("igloader: stat %q: %w", filePath, err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("igloader: expected .tgz file at %q, got directory", filePath)
	}
	if info.Size() == 0 {
		return nil, ErrEmptyArchive
	}
	if info.Size() > limits.MaxArchiveBytes {
		return nil, fmt.Errorf("%w: %q is %d bytes, limit %d", ErrArchiveTooLarge, filePath, info.Size(), limits.MaxArchiveBytes)
	}
	f, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("igloader: open %q: %w", filePath, err)
	}
	defer f.Close()

	pkg, err := LoadTGZReader(f, opts)
	if err != nil {
		return nil, err
	}
	pkg.Source = filePath
	return pkg, nil
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// LoadTGZReader streams, unpacks, and validates an HL7 FHIR NPM .tgz archive from r.
func LoadTGZReader(r io.Reader, opts Options) (*Package, error) {
	if r == nil {
		return nil, ErrEmptyArchive
	}
	limits := normalizeLimits(opts.Limits)

	cr := &countingReader{r: io.LimitReader(r, limits.MaxArchiveBytes+1)}
	gz, err := gzip.NewReader(cr)
	if err != nil {
		if cr.n == 0 && errors.Is(err, io.EOF) {
			return nil, ErrEmptyArchive
		}
		if cr.n > limits.MaxArchiveBytes {
			return nil, fmt.Errorf("%w: limit %d bytes", ErrArchiveTooLarge, limits.MaxArchiveBytes)
		}
		return nil, fmt.Errorf("igloader: open gzip stream: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	var files []rawFileEntry
	var totalDecompressed int64
	entryCount := 0

	for {
		hdr, err := tr.Next()
		if cr.n > limits.MaxArchiveBytes {
			return nil, fmt.Errorf("%w: limit %d bytes", ErrArchiveTooLarge, limits.MaxArchiveBytes)
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("igloader: read tar header: %w", err)
		}

		if hdr.Typeflag == tar.TypeXHeader || hdr.Typeflag == tar.TypeXGlobalHeader {
			continue
		}

		entryCount++
		if entryCount > limits.MaxEntries {
			return nil, fmt.Errorf("%w: limit %d entries", ErrTooManyEntries, limits.MaxEntries)
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if _, err := sanitizeArchivePath(hdr.Name); err != nil {
				return nil, err
			}
			continue
		case tar.TypeReg, 0:
			// Regular file — validated below
		default:
			return nil, fmt.Errorf("%w: %q has disallowed tar type %d", ErrUnsafeTarEntry, hdr.Name, hdr.Typeflag)
		}

		cleanPath, err := sanitizeArchivePath(hdr.Name)
		if err != nil {
			return nil, err
		}

		if hdr.Size < 0 {
			return nil, fmt.Errorf("%w: negative tar size %d for %q", ErrFileTooLarge, hdr.Size, cleanPath)
		}
		if hdr.Size > limits.MaxSingleFileBytes {
			return nil, fmt.Errorf("%w: %q is %d bytes, limit %d", ErrFileTooLarge, cleanPath, hdr.Size, limits.MaxSingleFileBytes)
		}

		data, err := io.ReadAll(io.LimitReader(tr, limits.MaxSingleFileBytes+1))
		if err != nil {
			return nil, fmt.Errorf("igloader: read %q from archive: %w", cleanPath, err)
		}
		if cr.n > limits.MaxArchiveBytes {
			return nil, fmt.Errorf("%w: limit %d bytes", ErrArchiveTooLarge, limits.MaxArchiveBytes)
		}
		if int64(len(data)) > limits.MaxSingleFileBytes {
			return nil, fmt.Errorf("%w: %q exceeds limit %d bytes", ErrFileTooLarge, cleanPath, limits.MaxSingleFileBytes)
		}
		totalDecompressed += int64(len(data))
		if totalDecompressed > limits.MaxDecompressedBytes {
			return nil, fmt.Errorf("%w: exceeds limit %d bytes", ErrDecompressedSizeExceeded, limits.MaxDecompressedBytes)
		}

		files = append(files, rawFileEntry{
			cleanPath: cleanPath,
			data:      data,
		})
	}

	return buildPackageFromEntries(files, "tgz", opts)
}

// LoadDir loads an unpacked HL7 FHIR NPM package directory containing either
// <dirPath>/package/package.json or <dirPath>/package.json.
func LoadDir(dirPath string, opts Options) (*Package, error) {
	limits := normalizeLimits(opts.Limits)
	info, err := os.Stat(dirPath)
	if err != nil {
		return nil, fmt.Errorf("igloader: stat directory %q: %w", dirPath, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("igloader: expected directory at %q", dirPath)
	}

	var files []rawFileEntry
	var totalBytes int64
	entryCount := 0

	var walk func(absDir, relPrefix string) error
	walk = func(absDir, relPrefix string) error {
		entries, err := os.ReadDir(absDir)
		if err != nil {
			return fmt.Errorf("igloader: read directory %q: %w", absDir, err)
		}
		for _, d := range entries {
			entryCount++
			if entryCount > limits.MaxEntries {
				return fmt.Errorf("%w: limit %d entries", ErrTooManyEntries, limits.MaxEntries)
			}
			if d.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("%w: %q", ErrUnsafeSymlink, filepath.Join(absDir, d.Name()))
			}
			relName := d.Name()
			if relPrefix != "" {
				relName = relPrefix + "/" + d.Name()
			}
			cleanRel, err := sanitizeArchivePath(relName)
			if err != nil {
				return err
			}
			childAbs := filepath.Join(absDir, d.Name())
			if d.IsDir() {
				if err := walk(childAbs, cleanRel); err != nil {
					return err
				}
				continue
			}
			if !d.Type().IsRegular() {
				return fmt.Errorf("%w: non-regular file %q", ErrUnsafeTarEntry, childAbs)
			}
			fInfo, err := d.Info()
			if err != nil {
				return fmt.Errorf("igloader: stat %q: %w", childAbs, err)
			}
			if fInfo.Size() > limits.MaxSingleFileBytes {
				return fmt.Errorf("%w: %q is %d bytes, limit %d", ErrFileTooLarge, cleanRel, fInfo.Size(), limits.MaxSingleFileBytes)
			}
			f, err := os.Open(childAbs)
			if err != nil {
				return fmt.Errorf("igloader: open %q: %w", childAbs, err)
			}
			data, readErr := io.ReadAll(io.LimitReader(f, limits.MaxSingleFileBytes+1))
			_ = f.Close()
			if readErr != nil {
				return fmt.Errorf("igloader: read %q: %w", childAbs, readErr)
			}
			if int64(len(data)) > limits.MaxSingleFileBytes {
				return fmt.Errorf("%w: %q exceeds limit %d bytes", ErrFileTooLarge, cleanRel, limits.MaxSingleFileBytes)
			}
			totalBytes += int64(len(data))
			if totalBytes > limits.MaxDecompressedBytes {
				return fmt.Errorf("%w: exceeds limit %d bytes", ErrDecompressedSizeExceeded, limits.MaxDecompressedBytes)
			}
			files = append(files, rawFileEntry{
				cleanPath: cleanRel,
				data:      data,
			})
		}
		return nil
	}

	if err := walk(dirPath, ""); err != nil {
		return nil, err
	}

	pkg, err := buildPackageFromEntries(files, dirPath, opts)
	if err != nil {
		return nil, err
	}
	pkg.Source = dirPath
	return pkg, nil
}

// LoadPath loads one or more FHIR R4 NPM packages from targetPath, which may be:
//   - a single .tgz / .tar.gz archive file,
//   - an unpacked IG directory (containing package/package.json or package.json), or
//   - a directory containing multiple .tgz / .tar.gz archives and/or unpacked IG subdirectories.
//
// Loaded packages are returned in deterministic topological dependency order.
func LoadPath(targetPath string, opts Options) ([]*Package, error) {
	trimmed := strings.TrimSpace(targetPath)
	if trimmed == "" {
		return nil, errors.New("igloader: empty target path")
	}
	info, err := os.Stat(trimmed)
	if err != nil {
		return nil, fmt.Errorf("igloader: stat %q: %w", trimmed, err)
	}
	if !info.IsDir() {
		pkg, err := LoadTGZFile(trimmed, opts)
		if err != nil {
			return nil, err
		}
		return []*Package{pkg}, nil
	}

	if hasManifestFile(trimmed) {
		pkg, err := LoadDir(trimmed, opts)
		if err != nil {
			return nil, err
		}
		return []*Package{pkg}, nil
	}

	entries, err := os.ReadDir(trimmed)
	if err != nil {
		return nil, fmt.Errorf("igloader: read bundle directory %q: %w", trimmed, err)
	}
	var pkgs []*Package
	for _, d := range entries {
		if strings.HasPrefix(d.Name(), ".") {
			continue
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("%w: %q", ErrUnsafeSymlink, filepath.Join(trimmed, d.Name()))
		}
		childPath := filepath.Join(trimmed, d.Name())
		if d.IsDir() {
			if hasManifestFile(childPath) {
				pkg, err := LoadDir(childPath, opts)
				if err != nil {
					return nil, err
				}
				pkgs = append(pkgs, pkg)
			}
			continue
		}
		lower := strings.ToLower(d.Name())
		if strings.HasSuffix(lower, ".tgz") || strings.HasSuffix(lower, ".tar.gz") {
			pkg, err := LoadTGZFile(childPath, opts)
			if err != nil {
				return nil, err
			}
			pkgs = append(pkgs, pkg)
		}
	}
	if len(pkgs) == 0 {
		return nil, fmt.Errorf("%w: no package.json or .tgz archives found in %q", ErrMissingManifest, trimmed)
	}
	return SortByDependencies(pkgs)
}

// LoadPaths loads FHIR R4 NPM packages from multiple file or directory paths (supporting
// comma-separated entries), deduplicates identical name#version coordinates, and returns
// them in deterministic topological dependency order.
func LoadPaths(paths []string, opts Options) ([]*Package, error) {
	var expanded []string
	for _, raw := range paths {
		for part := range strings.SplitSeq(raw, ",") {
			if trimmed := strings.TrimSpace(part); trimmed != "" {
				expanded = append(expanded, trimmed)
			}
		}
	}
	if len(expanded) == 0 {
		return nil, nil
	}

	var all []*Package
	seenCoord := make(map[string]int)
	for _, p := range expanded {
		loaded, err := LoadPath(p, opts)
		if err != nil {
			return nil, err
		}
		for _, pkg := range loaded {
			coord := strings.ToLower(pkg.PackageCoord())
			if idx, exists := seenCoord[coord]; exists {
				all[idx] = pkg
			} else {
				seenCoord[coord] = len(all)
				all = append(all, pkg)
			}
		}
	}
	return SortByDependencies(all)
}

func hasManifestFile(dirPath string) bool {
	for _, rel := range []string{filepath.Join("package", "package.json"), "package.json"} {
		st, err := os.Stat(filepath.Join(dirPath, rel))
		if err == nil && !st.IsDir() {
			return true
		}
	}
	return false
}

func sanitizeArchivePath(raw string) (string, error) {
	if raw == "" || strings.ContainsRune(raw, '\x00') || strings.ContainsRune(raw, '\\') {
		return "", fmt.Errorf("%w: %q", ErrUnsafeArchivePath, raw)
	}
	trimmed := raw
	for strings.HasPrefix(trimmed, "./") {
		trimmed = strings.TrimPrefix(trimmed, "./")
	}
	trimmed = strings.TrimSuffix(trimmed, "/")
	if trimmed == "" || trimmed == "." {
		return ".", nil
	}
	if strings.HasPrefix(trimmed, "/") || strings.HasPrefix(raw, "/") {
		return "", fmt.Errorf("%w: absolute path %q", ErrUnsafeArchivePath, raw)
	}
	segments := strings.Split(trimmed, "/")
	if len(segments[0]) >= 2 && segments[0][1] == ':' {
		return "", fmt.Errorf("%w: volume prefix in %q", ErrUnsafeArchivePath, raw)
	}
	for _, seg := range segments {
		if seg == ".." {
			return "", fmt.Errorf("%w: parent traversal in %q", ErrUnsafeArchivePath, raw)
		}
	}
	cleaned := path.Clean(trimmed)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.HasPrefix(cleaned, "/") {
		return "", fmt.Errorf("%w: %q", ErrUnsafeArchivePath, raw)
	}
	return cleaned, nil
}

type rawManifestJSON struct {
	Name            string            `json:"name"`
	Version         string            `json:"version"`
	FHIRVersions    []string          `json:"fhirVersions"`
	FHIRVersionList []string          `json:"fhir-version-list"`
	Canonical       string            `json:"canonical"`
	URL             string            `json:"url"`
	Title           string            `json:"title"`
	Description     string            `json:"description"`
	Type            string            `json:"type"`
	Author          string            `json:"author"`
	License         string            `json:"license"`
	Dependencies    map[string]string `json:"dependencies"`
}

func parseAndValidateManifest(data []byte) (Manifest, error) {
	var raw rawManifestJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return Manifest{}, fmt.Errorf("%w: %v", ErrInvalidManifest, err)
	}
	name := strings.TrimSpace(raw.Name)
	ver := strings.TrimSpace(raw.Version)
	if name == "" {
		return Manifest{}, fmt.Errorf("%w: missing required 'name' field", ErrInvalidManifest)
	}
	if ver == "" {
		return Manifest{}, fmt.Errorf("%w: missing required 'version' field for %q", ErrInvalidManifest, name)
	}

	var versions []string
	seenVer := make(map[string]bool)
	for _, v := range append(raw.FHIRVersions, raw.FHIRVersionList...) {
		tv := strings.TrimSpace(v)
		if tv != "" && !seenVer[tv] {
			seenVer[tv] = true
			versions = append(versions, tv)
		}
	}

	if len(versions) > 0 {
		hasR4 := false
		for _, v := range versions {
			if isR4VersionToken(v) {
				hasR4 = true
				break
			}
		}
		if !hasR4 {
			return Manifest{}, fmt.Errorf("%w: package %s#%s targets %v", ErrIncompatibleFHIRVersion, name, ver, versions)
		}
	} else {
		if hasNonR4CoreDependencyOnly(raw.Dependencies) {
			return Manifest{}, fmt.Errorf("%w: package %s#%s depends on non-R4 FHIR core", ErrIncompatibleFHIRVersion, name, ver)
		}
		versions = []string{"4.0.1"}
	}

	deps := make(map[string]string, len(raw.Dependencies))
	for k, v := range raw.Dependencies {
		tk := strings.TrimSpace(k)
		if tk != "" {
			deps[tk] = strings.TrimSpace(v)
		}
	}

	canonical := strings.TrimSpace(raw.Canonical)
	if canonical == "" {
		canonical = strings.TrimSpace(raw.URL)
	}

	return Manifest{
		Name:         name,
		Version:      ver,
		FHIRVersions: versions,
		Canonical:    canonical,
		URL:          strings.TrimSpace(raw.URL),
		Title:        strings.TrimSpace(raw.Title),
		Description:  strings.TrimSpace(raw.Description),
		Type:         strings.TrimSpace(raw.Type),
		Author:       strings.TrimSpace(raw.Author),
		License:      strings.TrimSpace(raw.License),
		Dependencies: deps,
	}, nil
}

func isR4VersionToken(v string) bool {
	lv := strings.ToLower(strings.TrimSpace(v))
	if lv == "4.0" || lv == "4.0.x" || lv == "r4" {
		return true
	}
	return strings.HasPrefix(lv, "4.0.")
}

func hasNonR4CoreDependencyOnly(deps map[string]string) bool {
	if len(deps) == 0 {
		return false
	}
	hasR4 := false
	hasOtherMajor := false
	for k := range deps {
		lk := strings.ToLower(strings.TrimSpace(k))
		switch {
		case strings.HasPrefix(lk, "hl7.fhir.r4."):
			hasR4 = true
		case strings.HasPrefix(lk, "hl7.fhir.r2."),
			strings.HasPrefix(lk, "hl7.fhir.r3."),
			strings.HasPrefix(lk, "hl7.fhir.r4b."),
			strings.HasPrefix(lk, "hl7.fhir.r5."),
			strings.HasPrefix(lk, "hl7.fhir.r6."):
			hasOtherMajor = true
		}
	}
	return hasOtherMajor && !hasR4
}

func buildPackageFromEntries(files []rawFileEntry, source string, opts Options) (*Package, error) {
	var manifestEntry *rawFileEntry
	manifestRootPrefix := ""
	for i := range files {
		if files[i].cleanPath == "package/package.json" {
			manifestEntry = &files[i]
			manifestRootPrefix = "package/"
			break
		}
	}
	if manifestEntry == nil {
		for i := range files {
			if files[i].cleanPath == "package.json" {
				manifestEntry = &files[i]
				manifestRootPrefix = ""
				break
			}
		}
	}
	if manifestEntry == nil {
		return nil, ErrMissingManifest
	}

	manifest, err := parseAndValidateManifest(manifestEntry.data)
	if err != nil {
		return nil, err
	}

	var artifacts []Artifact
	for _, f := range files {
		if f.cleanPath == manifestEntry.cleanPath {
			continue
		}
		var rel string
		if manifestRootPrefix != "" {
			var ok bool
			rel, ok = strings.CutPrefix(f.cleanPath, manifestRootPrefix)
			if !ok {
				continue
			}
		} else {
			rel = f.cleanPath
		}

		if shouldSkipPackageRelativePath(rel) {
			continue
		}
		inExampleDir := isExampleRelativePath(rel)
		if inExampleDir && !opts.LoadExamples {
			continue
		}

		var parsed map[string]any
		if err := json.Unmarshal(f.data, &parsed); err != nil {
			return nil, fmt.Errorf("igloader: parse %q in %s#%s: %w", f.cleanPath, manifest.Name, manifest.Version, err)
		}
		rt, _ := parsed["resourceType"].(string)
		rt = strings.TrimSpace(rt)
		if rt == "" {
			continue
		}

		isEx := inExampleDir || !IsConformanceResourceType(rt)
		if isEx && !opts.LoadExamples {
			continue
		}

		art := Artifact{
			Filename:     f.cleanPath,
			ResourceType: rt,
			ID:           stringField(parsed, "id"),
			URL:          stringField(parsed, "url"),
			Version:      stringField(parsed, "version"),
			Name:         stringField(parsed, "name"),
			Status:       stringField(parsed, "status"),
			Kind:         stringField(parsed, "kind"),
			Type:         stringField(parsed, "type"),
			Abstract:     boolField(parsed, "abstract"),
			IsExample:    isEx,
			RawJSON:      bytes.Clone(f.data),
			Parsed:       parsed,
		}
		artifacts = append(artifacts, art)
	}

	sortArtifactsForRegistration(artifacts)

	return &Package{
		Manifest:  manifest,
		Source:    source,
		LoadedAt:  time.Now().UTC(),
		Artifacts: artifacts,
	}, nil
}

func shouldSkipPackageRelativePath(rel string) bool {
	if rel == "" || rel == "." {
		return true
	}
	lower := strings.ToLower(rel)
	if !strings.HasSuffix(lower, ".json") {
		return true
	}
	segments := strings.Split(rel, "/")
	base := segments[len(segments)-1]
	lowerBase := strings.ToLower(base)
	if strings.HasPrefix(base, ".") || lowerBase == "package.json" || lowerBase == ".index.json" {
		return true
	}
	for _, seg := range segments[:len(segments)-1] {
		ls := strings.ToLower(seg)
		if strings.HasPrefix(seg, ".") || ls == "other" || ls == "xml" || ls == "openapi" || ls == "rdf" {
			return true
		}
	}
	return false
}

func isExampleRelativePath(rel string) bool {
	segments := strings.Split(rel, "/")
	for _, seg := range segments[:len(segments)-1] {
		ls := strings.ToLower(seg)
		if ls == "example" || ls == "examples" {
			return true
		}
	}
	return false
}

// IsConformanceResourceType reports whether rt is an HL7 FHIR R4 conformance,
// terminology, or definitional artifact type registered during IG installation.
func IsConformanceResourceType(rt string) bool {
	switch strings.TrimSpace(rt) {
	case "CodeSystem",
		"ValueSet",
		"ConceptMap",
		"NamingSystem",
		"TerminologyCapabilities",
		"StructureDefinition",
		"SearchParameter",
		"OperationDefinition",
		"CapabilityStatement",
		"CompartmentDefinition",
		"StructureMap",
		"GraphDefinition",
		"MessageDefinition",
		"ImplementationGuide",
		"ExampleScenario",
		"Questionnaire",
		"PlanDefinition",
		"ActivityDefinition",
		"Measure",
		"Library":
		return true
	default:
		return false
	}
}

func artifactTypePriority(a Artifact) int {
	if a.IsExample {
		return 100
	}
	switch a.ResourceType {
	case "CodeSystem":
		return 10
	case "ValueSet":
		return 20
	case "ConceptMap":
		return 30
	case "NamingSystem", "TerminologyCapabilities":
		return 35
	case "StructureDefinition":
		// Register Extension and datatype StructureDefinitions before Resource profiles
		if a.Type == "Extension" || a.Kind == "complex-type" || a.Kind == "primitive-type" {
			return 40
		}
		return 45
	case "SearchParameter":
		return 50
	case "OperationDefinition", "CompartmentDefinition", "StructureMap", "GraphDefinition", "MessageDefinition":
		return 60
	case "CapabilityStatement":
		return 70
	case "Questionnaire", "PlanDefinition", "ActivityDefinition", "Measure", "Library":
		return 75
	case "ImplementationGuide":
		return 80
	default:
		return 90
	}
}

func sortArtifactsForRegistration(artifacts []Artifact) {
	sort.SliceStable(artifacts, func(i, j int) bool {
		pi, pj := artifactTypePriority(artifacts[i]), artifactTypePriority(artifacts[j])
		if pi != pj {
			return pi < pj
		}
		if artifacts[i].URL != artifacts[j].URL {
			return artifacts[i].URL < artifacts[j].URL
		}
		return artifacts[i].Filename < artifacts[j].Filename
	})
}

// SortByDependencies returns a new slice of packages sorted in deterministic topological
// dependency order so every dependency present in pkgs is installed before its dependents.
// External base dependencies not present in pkgs (such as hl7.fhir.r4.core) are ignored.
// Returns ErrDependencyCycle if a circular dependency exists among pkgs.
func SortByDependencies(pkgs []*Package) ([]*Package, error) {
	n := len(pkgs)
	if n <= 1 {
		out := make([]*Package, n)
		copy(out, pkgs)
		return out, nil
	}

	byName := make(map[string][]int, n)
	for idx, pkg := range pkgs {
		if pkg == nil {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(pkg.Manifest.Name))
		byName[key] = append(byName[key], idx)
	}

	adj := make([][]int, n)
	inDegree := make([]int, n)
	for idx, pkg := range pkgs {
		if pkg == nil {
			continue
		}
		seenDep := make(map[int]bool)
		for depName := range pkg.Manifest.Dependencies {
			depKey := strings.ToLower(strings.TrimSpace(depName))
			for _, depIdx := range byName[depKey] {
				if depIdx == idx || seenDep[depIdx] {
					continue
				}
				seenDep[depIdx] = true
				adj[depIdx] = append(adj[depIdx], idx)
				inDegree[idx]++
			}
		}
	}

	// Ready queue ordered deterministically by PackageCoord() then original index.
	var ready []int
	for i := range n {
		if inDegree[i] == 0 {
			ready = append(ready, i)
		}
	}
	sortReady := func() {
		sort.Slice(ready, func(a, b int) bool {
			ca := strings.ToLower(pkgs[ready[a]].PackageCoord())
			cb := strings.ToLower(pkgs[ready[b]].PackageCoord())
			if ca != cb {
				return ca < cb
			}
			return ready[a] < ready[b]
		})
	}
	sortReady()

	sorted := make([]*Package, 0, n)
	for len(ready) > 0 {
		curr := ready[0]
		ready = ready[1:]
		sorted = append(sorted, pkgs[curr])

		added := false
		for _, next := range adj[curr] {
			inDegree[next]--
			if inDegree[next] == 0 {
				ready = append(ready, next)
				added = true
			}
		}
		if added {
			sortReady()
		}
	}

	if len(sorted) != n {
		var cycleNames []string
		for i := range n {
			if inDegree[i] > 0 && pkgs[i] != nil {
				cycleNames = append(cycleNames, pkgs[i].PackageCoord())
			}
		}
		sort.Strings(cycleNames)
		return nil, fmt.Errorf("%w: involving %s", ErrDependencyCycle, strings.Join(cycleNames, " <-> "))
	}

	return sorted, nil
}

func stringField(m map[string]any, key string) string {
	if s, ok := m[key].(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

func boolField(m map[string]any, key string) bool {
	b, _ := m[key].(bool)
	return b
}

// Ensure fs is referenced for documentation/interface parity if needed.
var _ fs.DirEntry
