package igloader

import "testing"

func FuzzLoadTGZBytes(f *testing.F) {
	f.Add(buildTestTGZ(f, []tarSpecEntry{
		{
			name: "package/package.json",
			body: `{"name":"hl7.fhir.uv.fuzz","version":"1.0.0","fhirVersions":["4.0.1"],"canonical":"http://example.org/fhir/fuzz"}`,
		},
		{
			name: "package/StructureDefinition-fuzz.json",
			body: `{"resourceType":"StructureDefinition","id":"fuzz","url":"http://example.org/fhir/StructureDefinition/fuzz","type":"Patient","kind":"resource"}`,
		},
		{
			name: "package/examples/Patient-1.json",
			body: `{"resourceType":"Patient","id":"p1"}`,
		},
	}))
	f.Add(buildTestTGZ(f, []tarSpecEntry{
		{
			name: "package/package.json",
			body: `{"name":"hl7.fhir.uv.r5","version":"1.0.0","fhirVersions":["5.0.0"]}`,
		},
	}))
	f.Add(buildTestTGZ(f, []tarSpecEntry{
		{
			name: "../evil.json",
			body: `{"name":"evil","version":"1.0.0"}`,
		},
	}))
	f.Add([]byte{})
	f.Add([]byte{0x1f, 0x8b, 0x08, 0x00, 0x00})

	f.Fuzz(func(t *testing.T, data []byte) {
		opts := Options{
			LoadExamples: true,
			Limits: Limits{
				MaxArchiveBytes:      1 << 20,
				MaxDecompressedBytes: 2 << 20,
				MaxSingleFileBytes:   512 << 10,
				MaxEntries:           128,
			},
		}
		pkg, err := LoadTGZBytes(data, opts)
		if err != nil {
			return
		}
		if pkg.Manifest.Name == "" || pkg.Manifest.Version == "" {
			t.Fatalf("loaded package missing required manifest name/version: %+v", pkg.Manifest)
		}
		_ = pkg.PackageCoord()
		_ = pkg.CanonicalURI()
		_ = pkg.VersionedCanonical()
		_ = pkg.ConformanceArtifacts()
		_ = pkg.ExampleArtifacts()
	})
}
