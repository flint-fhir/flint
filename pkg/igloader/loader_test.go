package igloader

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type tarSpecEntry struct {
	name     string
	typeflag byte
	body     string
	linkname string
}

func buildTestTGZ(t testing.TB, entries []tarSpecEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		tf := e.typeflag
		if tf == 0 {
			tf = tar.TypeReg
		}
		hdr := &tar.Header{
			Name:     e.name,
			Typeflag: tf,
			Mode:     0o644,
			Size:     int64(len(e.body)),
			Linkname: e.linkname,
		}
		if tf == tar.TypeDir {
			hdr.Mode = 0o755
			hdr.Size = 0
		}
		if tf == tar.TypeSymlink || tf == tar.TypeLink || tf == tar.TypeChar {
			hdr.Size = 0
		}
		require.NoError(t, tw.WriteHeader(hdr))
		if len(e.body) > 0 && tf == tar.TypeReg {
			_, err := tw.Write([]byte(e.body))
			require.NoError(t, err)
		}
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	return buf.Bytes()
}

func TestLoadTGZBytes_ValidPackageAndExampleFiltering(t *testing.T) {
	raw := buildTestTGZ(t, []tarSpecEntry{
		{name: "package/", typeflag: tar.TypeDir},
		{
			name: "package/package.json",
			body: `{
				"name": "hl7.fhir.uv.acme",
				"version": "1.2.0",
				"fhirVersions": ["4.0.1"],
				"canonical": "http://acme.org/fhir/ig/core",
				"title": "Acme Core IG",
				"description": "Acme FHIR R4 Implementation Guide",
				"type": "fhir.ig",
				"dependencies": {
					"hl7.fhir.r4.core": "4.0.1"
				}
			}`,
		},
		{
			name: "package/.index.json",
			body: `{"index-version": 1, "files": []}`,
		},
		{
			name: "package/other/validation-summary.json",
			body: `{not-even-valid-json-ignored-in-other-dir`,
		},
		{
			name: "package/README.md",
			body: `# Ignored markdown`,
		},
		{
			name: "package/StructureDefinition-acme-patient.json",
			body: `{
				"resourceType": "StructureDefinition",
				"id": "acme-patient",
				"url": "http://acme.org/fhir/ig/core/StructureDefinition/acme-patient",
				"version": "1.2.0",
				"name": "AcmePatient",
				"status": "active",
				"kind": "resource",
				"abstract": false,
				"type": "Patient"
			}`,
		},
		{
			name: "package/StructureDefinition-acme-ext.json",
			body: `{
				"resourceType": "StructureDefinition",
				"id": "acme-ext",
				"url": "http://acme.org/fhir/ig/core/StructureDefinition/acme-ext",
				"version": "1.2.0",
				"name": "AcmeExt",
				"status": "active",
				"kind": "complex-type",
				"abstract": false,
				"type": "Extension"
			}`,
		},
		{
			name: "package/SearchParameter-acme-mrn.json",
			body: `{
				"resourceType": "SearchParameter",
				"id": "acme-mrn",
				"url": "http://acme.org/fhir/ig/core/SearchParameter/acme-mrn",
				"code": "acme-mrn",
				"base": ["Patient"],
				"type": "token",
				"status": "active",
				"expression": "Patient.identifier"
			}`,
		},
		{
			name: "package/ValueSet-acme-codes.json",
			body: `{
				"resourceType": "ValueSet",
				"id": "acme-codes",
				"url": "http://acme.org/fhir/ig/core/ValueSet/acme-codes",
				"status": "active"
			}`,
		},
		{
			name: "package/CodeSystem-acme-cs.json",
			body: `{
				"resourceType": "CodeSystem",
				"id": "acme-cs",
				"url": "http://acme.org/fhir/ig/core/CodeSystem/acme-cs",
				"status": "active",
				"concept": [{"code": "A1", "display": "Alpha One"}]
			}`,
		},
		{
			name: "package/ConceptMap-acme-cm.json",
			body: `{
				"resourceType": "ConceptMap",
				"id": "acme-cm",
				"url": "http://acme.org/fhir/ig/core/ConceptMap/acme-cm",
				"status": "active"
			}`,
		},
		{
			name: "package/ImplementationGuide-acme.json",
			body: `{
				"resourceType": "ImplementationGuide",
				"id": "acme",
				"url": "http://acme.org/fhir/ig/core/ImplementationGuide/acme",
				"status": "active"
			}`,
		},
		{
			name: "package/CapabilityStatement-acme-server.json",
			body: `{
				"resourceType": "CapabilityStatement",
				"id": "acme-server",
				"url": "http://acme.org/fhir/ig/core/CapabilityStatement/acme-server",
				"status": "active",
				"fhirVersion": "4.0.1"
			}`,
		},
		{
			name: "package/custom-tool-metadata.json",
			body: `{"tool": "ig-publisher"}`,
		},
		{
			name: "package/Patient-inline-sample.json",
			body: `{"resourceType": "Patient", "id": "inline-pat"}`,
		},
		{
			name: "package/examples/Patient-ex1.json",
			body: `{"resourceType": "Patient", "id": "ex1"}`,
		},
	})

	pkgNoEx, err := LoadTGZBytes(raw, Options{LoadExamples: false})
	require.NoError(t, err)
	assert.Equal(t, "hl7.fhir.uv.acme#1.2.0", pkgNoEx.PackageCoord())
	assert.Equal(t, "http://acme.org/fhir/ig/core", pkgNoEx.CanonicalURI())
	assert.Equal(t, "http://acme.org/fhir/ig/core|1.2.0", pkgNoEx.VersionedCanonical())
	assert.Len(t, pkgNoEx.ConformanceArtifacts(), 8)
	assert.Empty(t, pkgNoEx.ExampleArtifacts())

	// Verify deterministic registration priority:
	// CodeSystem -> ValueSet -> ConceptMap -> Extension SD -> Resource SD -> SearchParameter -> CapabilityStatement -> ImplementationGuide
	conformance := pkgNoEx.ConformanceArtifacts()
	assert.Equal(t, "CodeSystem", conformance[0].ResourceType)
	assert.Equal(t, "ValueSet", conformance[1].ResourceType)
	assert.Equal(t, "ConceptMap", conformance[2].ResourceType)
	assert.Equal(t, "Extension", conformance[3].Type)
	assert.Equal(t, "Patient", conformance[4].Type)
	assert.Equal(t, "SearchParameter", conformance[5].ResourceType)
	assert.Equal(t, "CapabilityStatement", conformance[6].ResourceType)
	assert.Equal(t, "ImplementationGuide", conformance[7].ResourceType)
	assert.Len(t, pkgNoEx.ArtifactsByType("StructureDefinition"), 2)

	// With LoadExamples: true, both inline clinical sample and package/examples/* are loaded
	pkgWithEx, err := LoadTGZBytes(raw, Options{LoadExamples: true})
	require.NoError(t, err)
	assert.Len(t, pkgWithEx.ConformanceArtifacts(), 8)
	assert.Len(t, pkgWithEx.ExampleArtifacts(), 2)
	assert.Len(t, pkgWithEx.ArtifactsByType("Patient"), 2)
}

func TestPackageNilHelpersAndFallbackCanonical(t *testing.T) {
	var nilPkg *Package
	assert.Equal(t, "", nilPkg.PackageCoord())
	assert.Equal(t, "", nilPkg.CanonicalURI())
	assert.Equal(t, "", nilPkg.VersionedCanonical())
	assert.Nil(t, nilPkg.ConformanceArtifacts())
	assert.Nil(t, nilPkg.ExampleArtifacts())
	assert.Nil(t, nilPkg.ArtifactsByType("Patient"))

	// Fallback CanonicalURI from ImplementationGuide artifact when manifest omits canonical/url
	raw := buildTestTGZ(t, []tarSpecEntry{
		{
			name: "package.json",
			body: `{"name": "org.example.minimal", "version": "0.1.0", "fhir-version-list": ["4.0.0"]}`,
		},
		{
			name: "ImplementationGuide-min.json",
			body: `{"resourceType": "ImplementationGuide", "id": "min", "url": "http://example.org/fhir/ig/min"}`,
		},
		{
			name: "OperationDefinition-op.json",
			body: `{"resourceType": "OperationDefinition", "id": "op", "url": "http://example.org/op"}`,
		},
		{
			name: "Questionnaire-q.json",
			body: `{"resourceType": "Questionnaire", "id": "q", "url": "http://example.org/q"}`,
		},
		{
			name: "NamingSystem-ns.json",
			body: `{"resourceType": "NamingSystem", "id": "ns", "name": "NS"}`,
		},
	})
	pkg, err := LoadTGZBytes(raw, Options{})
	require.NoError(t, err)
	assert.Equal(t, "http://example.org/fhir/ig/min", pkg.CanonicalURI())
	assert.Equal(t, "http://example.org/fhir/ig/min|0.1.0", pkg.VersionedCanonical())
}

func TestManifestValidationAndFHIRVersionCompatibility(t *testing.T) {
	t.Run("rejects R5-only package", func(t *testing.T) {
		raw := buildTestTGZ(t, []tarSpecEntry{
			{
				name: "package/package.json",
				body: `{"name": "hl7.fhir.uv.r5only", "version": "1.0.0", "fhirVersions": ["5.0.0"]}`,
			},
		})
		_, err := LoadTGZBytes(raw, Options{})
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrIncompatibleFHIRVersion)
	})

	t.Run("rejects R5 core dependency when fhirVersions omitted", func(t *testing.T) {
		raw := buildTestTGZ(t, []tarSpecEntry{
			{
				name: "package/package.json",
				body: `{"name": "hl7.fhir.uv.r5dep", "version": "1.0.0", "dependencies": {"hl7.fhir.r5.core": "5.0.0"}}`,
			},
		})
		_, err := LoadTGZBytes(raw, Options{})
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrIncompatibleFHIRVersion)
	})

	t.Run("accepts omitted fhirVersions when compatible with R4", func(t *testing.T) {
		raw := buildTestTGZ(t, []tarSpecEntry{
			{
				name: "package/package.json",
				body: `{"name": "hl7.fhir.uv.implicitr4", "version": "1.0.0", "url": "http://example.org/ig", "dependencies": {"hl7.fhir.r4.core": "4.0.1"}}`,
			},
		})
		pkg, err := LoadTGZBytes(raw, Options{})
		require.NoError(t, err)
		assert.Equal(t, []string{"4.0.1"}, pkg.Manifest.FHIRVersions)
		assert.Equal(t, "http://example.org/ig", pkg.CanonicalURI())
	})

	t.Run("rejects missing manifest, invalid JSON, missing name or version", func(t *testing.T) {
		noManifest := buildTestTGZ(t, []tarSpecEntry{
			{name: "package/Patient.json", body: `{"resourceType": "Patient"}`},
		})
		_, err := LoadTGZBytes(noManifest, Options{})
		assert.ErrorIs(t, err, ErrMissingManifest)

		badManifest := buildTestTGZ(t, []tarSpecEntry{
			{name: "package/package.json", body: `{bad-json`},
		})
		_, err = LoadTGZBytes(badManifest, Options{})
		assert.ErrorIs(t, err, ErrInvalidManifest)

		noName := buildTestTGZ(t, []tarSpecEntry{
			{name: "package/package.json", body: `{"version": "1.0.0"}`},
		})
		_, err = LoadTGZBytes(noName, Options{})
		assert.ErrorIs(t, err, ErrInvalidManifest)

		noVersion := buildTestTGZ(t, []tarSpecEntry{
			{name: "package/package.json", body: `{"name": "pkg.nonver"}`},
		})
		_, err = LoadTGZBytes(noVersion, Options{})
		assert.ErrorIs(t, err, ErrInvalidManifest)

		badArtifact := buildTestTGZ(t, []tarSpecEntry{
			{name: "package/package.json", body: `{"name": "pkg.badart", "version": "1.0.0", "fhirVersions": ["4.0.1"]}`},
			{name: "package/StructureDefinition-broken.json", body: `{broken`},
		})
		_, err = LoadTGZBytes(badArtifact, Options{})
		require.Error(t, err)
	})
}

func TestSecurityHardening_ArchiveTraversalSymlinksAndLimits(t *testing.T) {
	t.Run("empty and non-gzip inputs", func(t *testing.T) {
		_, err := LoadTGZBytes(nil, Options{})
		assert.ErrorIs(t, err, ErrEmptyArchive)

		_, err = LoadTGZReader(nil, Options{})
		assert.ErrorIs(t, err, ErrEmptyArchive)

		_, err = LoadTGZReader(bytes.NewReader(nil), Options{})
		assert.ErrorIs(t, err, ErrEmptyArchive)

		_, err = LoadTGZBytes([]byte("not-a-gzip-stream"), Options{})
		require.Error(t, err)
	})

	t.Run("rejects path traversal and unsafe paths", func(t *testing.T) {
		_, nullErr := sanitizeArchivePath("package/\x00evil.json")
		assert.ErrorIs(t, nullErr, ErrUnsafeArchivePath)

		badPaths := []string{
			"../package.json",
			"package/../../etc/passwd",
			"/etc/passwd",
			"package\\evil.json",
			"C:/evil.json",
		}
		for _, bp := range badPaths {
			raw := buildTestTGZ(t, []tarSpecEntry{
				{name: bp, body: `{"name":"evil","version":"1.0.0"}`},
			})
			_, err := LoadTGZBytes(raw, Options{})
			require.Error(t, err, "expected error for path %q", bp)
			assert.ErrorIs(t, err, ErrUnsafeArchivePath)
		}
	})

	t.Run("rejects symlinks, hardlinks, and device tar headers", func(t *testing.T) {
		for _, tf := range []byte{tar.TypeSymlink, tar.TypeLink, tar.TypeChar} {
			raw := buildTestTGZ(t, []tarSpecEntry{
				{name: "package/package.json", body: `{"name":"x","version":"1.0.0","fhirVersions":["4.0.1"]}`},
				{name: "package/evil-link", typeflag: tf, linkname: "/etc/passwd"},
			})
			_, err := LoadTGZBytes(raw, Options{})
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrUnsafeTarEntry)
		}
	})

	t.Run("enforces MaxArchiveBytes, MaxSingleFileBytes, MaxDecompressedBytes, and MaxEntries", func(t *testing.T) {
		raw := buildTestTGZ(t, []tarSpecEntry{
			{name: "package/package.json", body: `{"name":"limits.test","version":"1.0.0","fhirVersions":["4.0.1"]}`},
			{name: "package/CodeSystem-1.json", body: `{"resourceType":"CodeSystem","id":"cs1","url":"http://example.org/cs1"}`},
			{name: "package/CodeSystem-2.json", body: `{"resourceType":"CodeSystem","id":"cs2","url":"http://example.org/cs2"}`},
		})

		_, err := LoadTGZBytes(raw, Options{Limits: Limits{MaxArchiveBytes: 10}})
		assert.ErrorIs(t, err, ErrArchiveTooLarge)

		_, err = LoadTGZReader(bytes.NewReader(raw), Options{Limits: Limits{MaxArchiveBytes: 10}})
		assert.ErrorIs(t, err, ErrArchiveTooLarge)

		_, err = LoadTGZBytes(raw, Options{Limits: Limits{MaxSingleFileBytes: 20}})
		assert.ErrorIs(t, err, ErrFileTooLarge)

		_, err = LoadTGZBytes(raw, Options{Limits: Limits{MaxDecompressedBytes: 90}})
		assert.ErrorIs(t, err, ErrDecompressedSizeExceeded)

		_, err = LoadTGZBytes(raw, Options{Limits: Limits{MaxEntries: 2}})
		assert.ErrorIs(t, err, ErrTooManyEntries)
	})
}

func TestLoadDirLoadPathLoadPathsAndDependencyResolution(t *testing.T) {
	root := t.TempDir()

	// Create 3 packages with dependencies:
	// pkg-c depends on pkg-b; pkg-b depends on pkg-a; pkg-a depends on hl7.fhir.r4.core (external)
	pkgATgz := filepath.Join(root, "bundle", "z-pkg-a.tgz")
	require.NoError(t, os.MkdirAll(filepath.Dir(pkgATgz), 0o755))
	rawA := buildTestTGZ(t, []tarSpecEntry{
		{
			name: "package/package.json",
			body: `{"name":"org.example.base","version":"1.0.0","fhirVersions":["4.0.1"],"canonical":"http://example.org/fhir/base","dependencies":{"hl7.fhir.r4.core":"4.0.1"}}`,
		},
		{
			name: "package/CodeSystem-base.json",
			body: `{"resourceType":"CodeSystem","id":"base-cs","url":"http://example.org/fhir/base/CodeSystem/base-cs"}`,
		},
	})
	require.NoError(t, os.WriteFile(pkgATgz, rawA, 0o644))

	// Unpacked directory package B inside bundle/m-pkg-b/package/package.json
	pkgBDir := filepath.Join(root, "bundle", "m-pkg-b")
	require.NoError(t, os.MkdirAll(filepath.Join(pkgBDir, "package"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(pkgBDir, "package", "package.json"), []byte(`{
		"name": "org.example.mid",
		"version": "1.1.0",
		"fhirVersions": ["4.0.1"],
		"canonical": "http://example.org/fhir/mid",
		"dependencies": {
			"org.example.base": "1.0.0"
		}
	}`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(pkgBDir, "package", "ValueSet-mid.json"), []byte(`{
		"resourceType": "ValueSet",
		"id": "mid-vs",
		"url": "http://example.org/fhir/mid/ValueSet/mid-vs"
	}`), 0o644))

	// Unpacked flat directory package C (no package/ subfolder) inside bundle/a-pkg-c/package.json
	pkgCDir := filepath.Join(root, "bundle", "a-pkg-c")
	require.NoError(t, os.MkdirAll(pkgCDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(pkgCDir, "package.json"), []byte(`{
		"name": "org.example.leaf",
		"version": "2.0.0",
		"fhirVersions": ["4.0.1"],
		"canonical": "http://example.org/fhir/leaf",
		"dependencies": {
			"org.example.mid": "1.1.0"
		}
	}`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(pkgCDir, "StructureDefinition-leaf.json"), []byte(`{
		"resourceType": "StructureDefinition",
		"id": "leaf-pat",
		"url": "http://example.org/fhir/leaf/StructureDefinition/leaf-pat",
		"type": "Patient",
		"kind": "resource"
	}`), 0o644))

	// Load entire bundle directory — lexical order is a-pkg-c, m-pkg-b, z-pkg-a,
	// so topological sort MUST reorder to: base -> mid -> leaf!
	pkgs, err := LoadPath(filepath.Join(root, "bundle"), Options{})
	require.NoError(t, err)
	require.Len(t, pkgs, 3)
	assert.Equal(t, "org.example.base#1.0.0", pkgs[0].PackageCoord())
	assert.Equal(t, "org.example.mid#1.1.0", pkgs[1].PackageCoord())
	assert.Equal(t, "org.example.leaf#2.0.0", pkgs[2].PackageCoord())

	// LoadPaths with comma-separated duplicates deduplicates and preserves topological order
	pkgsDedup, err := LoadPaths([]string{pkgCDir + "," + pkgBDir, pkgATgz, pkgBDir}, Options{})
	require.NoError(t, err)
	require.Len(t, pkgsDedup, 3)
	assert.Equal(t, "org.example.base#1.0.0", pkgsDedup[0].PackageCoord())
	assert.Equal(t, "org.example.mid#1.1.0", pkgsDedup[1].PackageCoord())
	assert.Equal(t, "org.example.leaf#2.0.0", pkgsDedup[2].PackageCoord())

	emptyRes, err := LoadPaths([]string{" , "}, Options{})
	require.NoError(t, err)
	assert.Nil(t, emptyRes)

	// Rejects symlink inside unpacked IG directory
	symDir := filepath.Join(root, "sym-ig")
	require.NoError(t, os.MkdirAll(symDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(symDir, "package.json"), []byte(`{"name":"sym.ig","version":"1.0.0","fhirVersions":["4.0.1"]}`), 0o644))
	require.NoError(t, os.Symlink(pkgATgz, filepath.Join(symDir, "linked.json")))
	_, err = LoadDir(symDir, Options{})
	assert.ErrorIs(t, err, ErrUnsafeSymlink)

	// Rejects dependency cycle
	_, err = SortByDependencies([]*Package{
		{Manifest: Manifest{Name: "cycle.alpha", Version: "1.0.0", Dependencies: map[string]string{"cycle.beta": "1.0.0"}}},
		{Manifest: Manifest{Name: "cycle.beta", Version: "1.0.0", Dependencies: map[string]string{"cycle.alpha": "1.0.0"}}},
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrDependencyCycle))
}
