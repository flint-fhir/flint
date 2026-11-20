package helm_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func chartDir(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs("flint")
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(abs, "Chart.yaml"))
	require.NoError(t, err)
	return abs
}

func runHelm(t *testing.T, args ...string) string {
	t.Helper()
	helmBin, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm binary not found in PATH; skipping Helm chart unit test")
	}
	cmd := exec.Command(helmBin, args...)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "helm %s failed:\n%s", strings.Join(args, " "), string(out))
	return string(out)
}

func splitManifests(rendered string) []string {
	rawDocs := strings.Split("\n"+rendered, "\n---")
	var docs []string
	for _, d := range rawDocs {
		trimmed := strings.TrimSpace(d)
		if trimmed != "" {
			docs = append(docs, trimmed)
		}
	}
	return docs
}

func findManifest(docs []string, kind string, nameSuffix string) string {
	wantKind := "kind: " + kind
	for _, doc := range docs {
		if !strings.Contains(doc, wantKind) {
			continue
		}
		if nameSuffix == "" || strings.Contains(doc, "name: "+nameSuffix) {
			return doc
		}
	}
	return ""
}

func TestHelmChart_LintPackageAndTemplateProfiles(t *testing.T) {
	dir := chartDir(t)

	// 1. Strict lint on default values
	lintDefault := runHelm(t, "lint", "--strict", dir)
	assert.Contains(t, lintDefault, "0 chart(s) failed")

	// 2. Default profile: stateless 2-replica HA Deployment, Service, ConfigMap, Secret, ServiceAccount, PDB
	defaultOut := runHelm(t, "template", "flint-rel", dir, "--namespace", "clinical")
	defaultDocs := splitManifests(defaultOut)

	dep := findManifest(defaultDocs, "Deployment", "flint-rel")
	require.NotEmpty(t, dep, "expected main Deployment in default template")
	assert.Contains(t, dep, "replicas: 2")
	assert.Contains(t, dep, "runAsNonRoot: true")
	assert.Contains(t, dep, "runAsUser: 10001")
	assert.Contains(t, dep, "readOnlyRootFilesystem: true")
	assert.Contains(t, dep, "allowPrivilegeEscalation: false")
	assert.Contains(t, dep, "- ALL")
	assert.Contains(t, dep, "path: /healthz")
	assert.Contains(t, dep, "path: /readyz")
	assert.Contains(t, dep, "topologyKey: topology.kubernetes.io/zone")
	assert.Contains(t, dep, "name: FLINT_POSTGRES_DSN")
	assert.Contains(t, dep, "name: FLINT_SIGNING_KEY_PEM")
	assert.Contains(t, dep, "name: FLINT_SHC_SIGNING_KEY_PEM")

	assert.NotEmpty(t, findManifest(defaultDocs, "Service", "flint-rel"))
	assert.NotEmpty(t, findManifest(defaultDocs, "ServiceAccount", "flint-rel"))
	assert.NotEmpty(t, findManifest(defaultDocs, "ConfigMap", "flint-rel"))
	assert.NotEmpty(t, findManifest(defaultDocs, "Secret", "flint-rel"))
	assert.NotEmpty(t, findManifest(defaultDocs, "PodDisruptionBudget", "flint-rel"))
	assert.Empty(t, findManifest(defaultDocs, "HorizontalPodAutoscaler", ""))
	assert.Empty(t, findManifest(defaultDocs, "Ingress", ""))
	assert.Empty(t, findManifest(defaultDocs, "ServiceMonitor", ""))
	assert.Empty(t, findManifest(defaultDocs, "Job", ""))

	// 3. Full HA + OpenTelemetry + HPA + Ingress + ServiceMonitor + Worker + Seed Job profile
	haArgs := []string{
		"--set", "autoscaling.enabled=true",
		"--set", "autoscaling.minReplicas=3",
		"--set", "autoscaling.maxReplicas=12",
		"--set", "telemetry.enabled=true",
		"--set", "telemetry.endpoint=http://otel-collector.observability:4318",
		"--set", "telemetry.protocol=http/protobuf",
		"--set", "telemetry.sampleRatio=0.5",
		"--set", "ingress.enabled=true",
		"--set", "ingress.className=nginx",
		"--set", "serviceMonitor.enabled=true",
		"--set", "worker.enabled=true",
		"--set", "seedJob.enabled=true",
		"--set", "terminology.serverURL=https://tx.fhir.org/r4",
	}
	runHelm(t, append([]string{"lint", "--strict", dir}, haArgs...)...)
	haOut := runHelm(t, append([]string{"template", "flint-ha", dir}, haArgs...)...)
	haDocs := splitManifests(haOut)

	haDep := findManifest(haDocs, "Deployment", "flint-ha\n")
	require.NotEmpty(t, haDep)
	assert.NotContains(t, haDep, "replicas:", "Deployment must omit static replicas when HPA is enabled")
	assert.Contains(t, haDep, "name: FLINT_OTLP_HEADERS")

	haCM := findManifest(haDocs, "ConfigMap", "flint-ha")
	require.NotEmpty(t, haCM)
	assert.Contains(t, haCM, `FLINT_OTLP_ENDPOINT: "http://otel-collector.observability:4318"`)
	assert.Contains(t, haCM, `FLINT_OTLP_PROTOCOL: "http/protobuf"`)
	assert.Contains(t, haCM, `FLINT_OTLP_SAMPLE_RATIO: "0.5"`)
	assert.Contains(t, haCM, `FLINT_TX_SERVER_URL: "https://tx.fhir.org/r4"`)

	assert.NotEmpty(t, findManifest(haDocs, "HorizontalPodAutoscaler", "flint-ha"))
	assert.NotEmpty(t, findManifest(haDocs, "Ingress", "flint-ha"))
	assert.NotEmpty(t, findManifest(haDocs, "ServiceMonitor", "flint-ha"))
	assert.NotEmpty(t, findManifest(haDocs, "Deployment", "flint-ha-worker"))
	seedJob := findManifest(haDocs, "Job", "flint-ha-seed")
	require.NotEmpty(t, seedJob)
	assert.Contains(t, seedJob, "/usr/local/bin/seed")

	// 4. External Secret + SQLite profile: chart omits managed Secret and mounts sqlite-data volume
	extArgs := []string{
		"--set", "existingSecret=flint-vault-secret",
		"--set", "store.backend=sqlite",
	}
	runHelm(t, append([]string{"lint", "--strict", dir}, extArgs...)...)
	extOut := runHelm(t, append([]string{"template", "flint-ext", dir}, extArgs...)...)
	extDocs := splitManifests(extOut)
	assert.Empty(t, findManifest(extDocs, "Secret", ""), "managed Secret must not render when existingSecret is set")
	extDep := findManifest(extDocs, "Deployment", "flint-ext")
	require.NotEmpty(t, extDep)
	assert.Contains(t, extDep, "name: flint-vault-secret")
	assert.Contains(t, extDep, "mountPath: /var/lib/flint")

	// 5. OCI chart archive packaging verification
	pkgDir := t.TempDir()
	runHelm(t, "package", dir, "--destination", pkgDir)
	entries, err := os.ReadDir(pkgDir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.True(t, strings.HasPrefix(entries[0].Name(), "flint-") && strings.HasSuffix(entries[0].Name(), ".tgz"))

	// 6. Optional strict kubeconform validation when FLINT_RUN_KUBECONFORM=1
	if os.Getenv("FLINT_RUN_KUBECONFORM") == "1" {
		kcBin, err := exec.LookPath("kubeconform")
		require.NoError(t, err, "FLINT_RUN_KUBECONFORM=1 requires kubeconform in PATH")
		kcCmd := exec.Command(kcBin, "-strict", "-summary", "-ignore-missing-schemas")
		kcCmd.Stdin = strings.NewReader(haOut)
		kcOut, err := kcCmd.CombinedOutput()
		require.NoError(t, err, "kubeconform failed:\n%s", string(kcOut))
	}
}
