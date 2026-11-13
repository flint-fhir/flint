package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/flint-fhir/flint/automq"
	"github.com/flint-fhir/flint/pkg/auth"
	"github.com/flint-fhir/flint/pkg/fhirutil"
	"github.com/flint-fhir/flint/pkg/igloader"
	"github.com/flint-fhir/flint/pkg/validation"
	"github.com/flint-fhir/flint/store"
)

var validFHIRIDChars = regexp.MustCompile(`[^A-Za-z0-9\-\.]`)

// InstallIGOptions configures how an HL7 FHIR R4 NPM Implementation Guide package
// is registered and optionally persisted/reindexed on a Server.
type InstallIGOptions struct {
	// Tenant is the target tenant ID when Persist or Reindex is enabled (defaults to "default").
	Tenant string
	// Persist writes conformance (and optional example) resources into s.store when s.store != nil.
	Persist bool
	// LoadExamples also persists example clinical resources present in pkg.Artifacts.
	LoadExamples bool
	// Reindex triggers a store search-index rebuild for resource types affected by newly installed SearchParameters.
	Reindex bool
}

// InstalledIGSummary describes an installed FHIR R4 NPM Implementation Guide package.
type InstalledIGSummary struct {
	Name               string         `json:"name"`
	Version            string         `json:"version"`
	Canonical          string         `json:"canonical,omitempty"`
	VersionedCanonical string         `json:"versionedCanonical,omitempty"`
	Title              string         `json:"title,omitempty"`
	FHIRVersions       []string       `json:"fhirVersions,omitempty"`
	LoadedAt           time.Time      `json:"loadedAt"`
	Source             string         `json:"source,omitempty"`
	ArtifactCounts     map[string]int `json:"artifactCounts"`
	TotalConformance   int            `json:"totalConformance"`
	TotalExamples      int            `json:"totalExamples"`
	PersistedResources int            `json:"persistedResources"`
	ReindexedResources int            `json:"reindexedResources"`
}

type igRegistryState struct {
	installed            []*InstalledIGSummary
	valueSets            map[string]map[string]any
	codeSystems          map[string]map[string]any
	conceptMaps          map[string]map[string]any
	conceptMapList       []map[string]any
	structureDefs        map[string]map[string]any
	capabilityStatements map[string]map[string]any
	supportedProfiles    map[string][]string
	igCanonicals         []string
}

func newIGRegistryState() *igRegistryState {
	return &igRegistryState{
		valueSets:            make(map[string]map[string]any),
		codeSystems:          make(map[string]map[string]any),
		conceptMaps:          make(map[string]map[string]any),
		structureDefs:        make(map[string]map[string]any),
		capabilityStatements: make(map[string]map[string]any),
		supportedProfiles:    make(map[string][]string),
	}
}

// SetAllowLocalIGPathInstall controls whether runtime HTTP POST /$install-ig requests
// may reference server-local filesystem paths via the "path" parameter (disabled by default).
func (s *Server) SetAllowLocalIGPathInstall(allow bool) {
	s.igMu.Lock()
	defer s.igMu.Unlock()
	s.allowLocalIGPaths = allow
}

// InstalledIGs returns a snapshot of all Implementation Guide packages installed on this server.
func (s *Server) InstalledIGs() []*InstalledIGSummary {
	s.igMu.RLock()
	defer s.igMu.RUnlock()
	if s.igState == nil || len(s.igState.installed) == 0 {
		return nil
	}
	out := make([]*InstalledIGSummary, len(s.igState.installed))
	for i, item := range s.igState.installed {
		cp := *item
		cp.FHIRVersions = append([]string(nil), item.FHIRVersions...)
		cp.ArtifactCounts = make(map[string]int, len(item.ArtifactCounts))
		for k, v := range item.ArtifactCounts {
			cp.ArtifactCounts[k] = v
		}
		out[i] = &cp
	}
	return out
}

// LoadIGPaths loads FHIR R4 NPM packages from file/directory paths in deterministic
// topological dependency order and registers all conformance artifacts on s.
func (s *Server) LoadIGPaths(ctx context.Context, paths []string, opts InstallIGOptions) ([]*InstalledIGSummary, error) {
	pkgs, err := igloader.LoadPaths(paths, igloader.Options{
		LoadExamples: opts.LoadExamples,
	})
	if err != nil {
		return nil, err
	}
	if len(pkgs) == 0 {
		return nil, nil
	}
	return s.InstallIGPackages(ctx, pkgs, opts)
}

// InstallIGPackages sorts pkgs in topological dependency order and installs each package.
func (s *Server) InstallIGPackages(ctx context.Context, pkgs []*igloader.Package, opts InstallIGOptions) ([]*InstalledIGSummary, error) {
	ordered, err := igloader.SortByDependencies(pkgs)
	if err != nil {
		return nil, err
	}
	summaries := make([]*InstalledIGSummary, 0, len(ordered))
	for _, pkg := range ordered {
		if pkg == nil {
			continue
		}
		sum, err := s.InstallIGPackage(ctx, pkg, opts)
		if err != nil {
			return summaries, err
		}
		summaries = append(summaries, sum)
	}
	return summaries, nil
}

// InstallIGPackage registers all conformance artifacts from pkg into the server's
// validation, search-parameter, terminology, and CapabilityStatement registries,
// optionally persisting resources to s.store and reindexing affected resource types.
func (s *Server) InstallIGPackage(ctx context.Context, pkg *igloader.Package, opts InstallIGOptions) (*InstalledIGSummary, error) {
	if pkg == nil {
		return nil, fmt.Errorf("ig package is nil")
	}

	tenant := strings.TrimSpace(opts.Tenant)
	if tenant == "" {
		tenant = "default"
	}

	counts := make(map[string]int)
	totalConformance := 0
	totalExamples := 0
	persistedCount := 0
	var spBaseTypes []string
	seenSPBase := make(map[string]bool)
	reindexAll := false

	valEng, _ := s.validator.(*validation.Engine)

	s.igMu.Lock()
	if s.igState == nil {
		s.igState = newIGRegistryState()
	}
	st := s.igState

	for _, art := range pkg.Artifacts {
		if art.IsExample && !opts.LoadExamples {
			continue
		}
		counts[art.ResourceType]++
		if art.IsExample {
			totalExamples++
			continue
		}
		totalConformance++

		switch art.ResourceType {
		case "CodeSystem":
			indexCanonicalArtifactMap(st.codeSystems, "CodeSystem", art)
		case "ValueSet":
			indexCanonicalArtifactMap(st.valueSets, "ValueSet", art)
		case "ConceptMap":
			indexCanonicalArtifactMap(st.conceptMaps, "ConceptMap", art)
			appendUniqueConceptMap(st, art.Parsed)
		case "StructureDefinition":
			indexCanonicalArtifactMap(st.structureDefs, "StructureDefinition", art)
			if valEng != nil && art.Parsed != nil {
				valEng.RegisterStructureDefinition(art.Parsed)
			}
			if !art.Abstract && strings.TrimSpace(art.URL) != "" {
				isResProfile := strings.EqualFold(art.Kind, "resource") ||
					(art.Kind == "" && art.Type != "" && art.Type != "Extension" && art.Type != "Element")
				if isResProfile && art.Type != "" && art.Type != "Resource" && art.Type != "DomainResource" {
					st.supportedProfiles[art.Type] = appendUniqueString(st.supportedProfiles[art.Type], strings.TrimSpace(art.URL))
				}
			}
		case "SearchParameter":
			registerSearchParameterMap(art.ID, art.Parsed)
			for _, baseRT := range extractSearchParamBases(art.Parsed) {
				if baseRT == "Resource" || baseRT == "DomainResource" {
					reindexAll = true
					continue
				}
				if !seenSPBase[baseRT] {
					seenSPBase[baseRT] = true
					spBaseTypes = append(spBaseTypes, baseRT)
				}
			}
		case "CapabilityStatement":
			indexCanonicalArtifactMap(st.capabilityStatements, "CapabilityStatement", art)
		case "ImplementationGuide":
			if u := strings.TrimSpace(art.URL); u != "" {
				st.igCanonicals = appendUniqueString(st.igCanonicals, u)
			}
		}
	}

	canon := pkg.CanonicalURI()
	if canon != "" {
		st.igCanonicals = appendUniqueString(st.igCanonicals, canon)
	}
	s.igMu.Unlock()

	if opts.Persist && s.store != nil {
		for _, art := range pkg.Artifacts {
			if art.IsExample && !opts.LoadExamples {
				continue
			}
			if _, ok := s.protoRegistry[art.ResourceType]; !ok {
				continue
			}
			if _, err := s.persistIGArtifact(ctx, tenant, art); err != nil {
				return nil, fmt.Errorf("persist %s (%s) from %s: %w", art.ResourceType, art.Filename, pkg.PackageCoord(), err)
			}
			persistedCount++
		}
	}

	reindexedTotal := 0
	if opts.Reindex && s.store != nil && (reindexAll || len(spBaseTypes) > 0) {
		if reindexAll {
			res, err := s.store.Reindex(ctx, store.ReindexParams{
				TenantID:   tenant,
				Extractors: s.extractors,
			})
			if err != nil {
				return nil, fmt.Errorf("reindex tenant %q after installing %s: %w", tenant, pkg.PackageCoord(), err)
			}
			reindexedTotal += res.ReindexedResources
		} else {
			sort.Strings(spBaseTypes)
			for _, rt := range spBaseTypes {
				if _, ok := s.protoRegistry[rt]; !ok {
					continue
				}
				res, err := s.store.Reindex(ctx, store.ReindexParams{
					TenantID:   tenant,
					ResType:    rt,
					Extractors: s.extractors,
				})
				if err != nil {
					return nil, fmt.Errorf("reindex %s in tenant %q after installing %s: %w", rt, tenant, pkg.PackageCoord(), err)
				}
				reindexedTotal += res.ReindexedResources
			}
		}
	}

	summary := &InstalledIGSummary{
		Name:               pkg.Manifest.Name,
		Version:            pkg.Manifest.Version,
		Canonical:          canon,
		VersionedCanonical: pkg.VersionedCanonical(),
		Title:              pkg.Manifest.Title,
		FHIRVersions:       append([]string(nil), pkg.Manifest.FHIRVersions...),
		LoadedAt:           pkg.LoadedAt,
		Source:             pkg.Source,
		ArtifactCounts:     counts,
		TotalConformance:   totalConformance,
		TotalExamples:      totalExamples,
		PersistedResources: persistedCount,
		ReindexedResources: reindexedTotal,
	}

	s.igMu.Lock()
	replaced := false
	for i, existing := range s.igState.installed {
		if strings.EqualFold(existing.Name, summary.Name) && existing.Version == summary.Version {
			s.igState.installed[i] = summary
			replaced = true
			break
		}
	}
	if !replaced {
		s.igState.installed = append(s.igState.installed, summary)
	}
	s.igMu.Unlock()

	if s.logger != nil {
		s.logger.Info("FHIR Implementation Guide package installed",
			"package", pkg.PackageCoord(),
			"canonical", canon,
			"conformance", totalConformance,
			"examples", totalExamples,
			"persisted", persistedCount,
			"reindexed", reindexedTotal,
		)
	}
	return summary, nil
}

func (s *Server) persistIGArtifact(ctx context.Context, tenant string, art igloader.Artifact) (string, error) {
	md, ok := s.protoRegistry[art.ResourceType]
	if !ok || md == nil {
		return "", nil
	}
	normalizedBody, err := fhirutil.NormalizeFHIRJSONToProtoJSON(md, art.RawJSON)
	if err != nil {
		return "", fmt.Errorf("normalize %s JSON: %w", art.ResourceType, err)
	}

	msg := dynamicpb.NewMessage(md)
	unmarshaler := protojson.UnmarshalOptions{DiscardUnknown: true}
	if err := unmarshaler.Unmarshal(normalizedBody, msg); err != nil {
		return "", fmt.Errorf("unmarshal %s proto: %w", art.ResourceType, err)
	}

	resID := sanitizeFHIRResourceID(extractProtoResourceID(msg))
	if resID == "" {
		resID = sanitizeFHIRResourceID(firstNonEmpty(art.ID, art.Name, tailOfCanonical(art.URL)))
	}
	if resID == "" {
		resID = generateID()
	}
	setProtoResourceID(msg, resID)

	protoBytes, err := proto.Marshal(msg)
	if err != nil {
		return "", fmt.Errorf("marshal %s proto: %w", art.ResourceType, err)
	}

	var idx *store.SearchIndexes
	if extractor, hasExt := s.extractors[art.ResourceType]; hasExt {
		idx, _ = extractor(tenant, resID, protoBytes)
	}

	if _, err := s.store.WriteResourceWithMeta(ctx, store.ResourceInput{
		TenantID:      tenant,
		ResType:       art.ResourceType,
		ResID:         resID,
		ResourceProto: protoBytes,
		SearchIndexes: idx,
	}); err != nil {
		return "", err
	}

	s.publishCDC(ctx, automq.Message{
		TenantID:  tenant,
		ResType:   art.ResourceType,
		ResID:     resID,
		ProtoData: protoBytes,
		Op:        "U",
	})
	return resID, nil
}

func sanitizeFHIRResourceID(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	s = validFHIRIDChars.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}

func tailOfCanonical(u string) string {
	u = strings.TrimSpace(u)
	if pipe := strings.IndexByte(u, '|'); pipe != -1 {
		u = u[:pipe]
	}
	u = strings.TrimRight(u, "/")
	if slash := strings.LastIndexByte(u, '/'); slash != -1 {
		return u[slash+1:]
	}
	return u
}

func indexCanonicalArtifactMap(target map[string]map[string]any, resType string, art igloader.Artifact) {
	if target == nil || art.Parsed == nil {
		return
	}
	u := strings.TrimSpace(art.URL)
	v := strings.TrimSpace(art.Version)
	id := strings.TrimSpace(art.ID)
	if u != "" {
		target[u] = art.Parsed
		if v != "" && !strings.Contains(u, "|") {
			target[u+"|"+v] = art.Parsed
		}
	}
	if id != "" {
		target[id] = art.Parsed
		target[resType+"/"+id] = art.Parsed
	}
}

func appendUniqueConceptMap(st *igRegistryState, parsed map[string]any) {
	if st == nil || parsed == nil {
		return
	}
	u, _ := parsed["url"].(string)
	v, _ := parsed["version"].(string)
	id, _ := parsed["id"].(string)
	for i, existing := range st.conceptMapList {
		eu, _ := existing["url"].(string)
		ev, _ := existing["version"].(string)
		eid, _ := existing["id"].(string)
		if (u != "" && u == eu && v == ev) || (u == "" && id != "" && id == eid) {
			st.conceptMapList[i] = parsed
			return
		}
	}
	st.conceptMapList = append(st.conceptMapList, parsed)
}

func appendUniqueString(slice []string, val string) []string {
	for _, s := range slice {
		if s == val {
			return slice
		}
	}
	return append(slice, val)
}

func extractSearchParamBases(m map[string]any) []string {
	if m == nil {
		return nil
	}
	var out []string
	switch b := m["base"].(type) {
	case []any:
		for _, item := range b {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
	case []string:
		for _, s := range b {
			if strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
	case string:
		if strings.TrimSpace(b) != "" {
			out = append(out, strings.TrimSpace(b))
		}
	}
	return out
}

func lookupVersionedArtifactMap(reg map[string]map[string]any, id, canonical string) map[string]any {
	if len(reg) == 0 {
		return nil
	}
	if c := strings.TrimSpace(canonical); c != "" {
		if m, ok := reg[c]; ok {
			return m
		}
		if pipe := strings.IndexByte(c, '|'); pipe != -1 {
			base := strings.TrimSpace(c[:pipe])
			reqVer := strings.TrimSpace(c[pipe+1:])
			if m, ok := reg[base]; ok {
				mv, _ := m["version"].(string)
				if strings.TrimSpace(mv) == "" || strings.TrimSpace(mv) == reqVer {
					return m
				}
			}
		}
	}
	if clean := strings.TrimSpace(id); clean != "" {
		if m, ok := reg[clean]; ok {
			return m
		}
	}
	return nil
}

func (s *Server) lookupIGValueSet(vsID, vsURL string) map[string]any {
	s.igMu.RLock()
	defer s.igMu.RUnlock()
	if s.igState == nil {
		return nil
	}
	return lookupVersionedArtifactMap(s.igState.valueSets, vsID, vsURL)
}

func (s *Server) lookupIGCodeSystem(csID, csURL string) map[string]any {
	s.igMu.RLock()
	defer s.igMu.RUnlock()
	if s.igState == nil {
		return nil
	}
	return lookupVersionedArtifactMap(s.igState.codeSystems, csID, csURL)
}

func (s *Server) lookupIGConceptMaps(cmID, cmURL string) ([]map[string]any, bool) {
	s.igMu.RLock()
	defer s.igMu.RUnlock()
	if s.igState == nil {
		return nil, false
	}
	if strings.TrimSpace(cmID) != "" || strings.TrimSpace(cmURL) != "" {
		if m := lookupVersionedArtifactMap(s.igState.conceptMaps, cmID, cmURL); m != nil {
			return []map[string]any{m}, true
		}
		return nil, false
	}
	if len(s.igState.conceptMapList) == 0 {
		return nil, false
	}
	cp := make([]map[string]any, len(s.igState.conceptMapList))
	copy(cp, s.igState.conceptMapList)
	return cp, false
}

func (s *Server) lookupIGCapabilityStatement(canonicalOrID string) map[string]any {
	s.igMu.RLock()
	defer s.igMu.RUnlock()
	if s.igState == nil {
		return nil
	}
	return lookupVersionedArtifactMap(s.igState.capabilityStatements, cleanID(canonicalOrID), canonicalOrID)
}

func (s *Server) lookupIGStructureDefinition(sdID, sdURL string) map[string]any {
	s.igMu.RLock()
	defer s.igMu.RUnlock()
	if s.igState == nil {
		return nil
	}
	return lookupVersionedArtifactMap(s.igState.structureDefs, sdID, sdURL)
}

func (s *Server) igSupportedProfilesForType(resType string) []string {
	s.igMu.RLock()
	defer s.igMu.RUnlock()
	if s.igState == nil {
		return nil
	}
	profs := s.igState.supportedProfiles[resType]
	if len(profs) == 0 {
		return nil
	}
	out := make([]string, len(profs))
	copy(out, profs)
	return out
}

func (s *Server) igImplementationGuideCanonicals() []string {
	s.igMu.RLock()
	defer s.igMu.RUnlock()
	if s.igState == nil || len(s.igState.igCanonicals) == 0 {
		return nil
	}
	out := make([]string, len(s.igState.igCanonicals))
	copy(out, s.igState.igCanonicals)
	return out
}

// validateCodeAgainstKnownValueSet enforces required StructureDefinition ValueSet bindings
// for IG-registered and store-persisted ValueSets/CodeSystems without falsely rejecting codes
// from open-world clinical terminology subsets (LOINC, SNOMED CT, RxNorm).
func (s *Server) validateCodeAgainstKnownValueSet(vsCanonical, system, code string) (bool, bool) {
	vsCanonical = strings.TrimSpace(vsCanonical)
	if vsCanonical == "" || strings.TrimSpace(code) == "" {
		return false, false
	}
	inlineVS := s.lookupIGValueSet("", vsCanonical)
	hasExplicitIGCS := s.lookupIGCodeSystem("", vsCanonical) != nil
	hasStoredVS := false
	if inlineVS == nil && !hasExplicitIGCS && s.store != nil {
		baseURL := vsCanonical
		if pipe := strings.IndexByte(baseURL, '|'); pipe != -1 {
			baseURL = strings.TrimSpace(baseURL[:pipe])
		}
		if sr, err := s.store.Search(context.Background(), store.SearchParams{
			TenantID: "default",
			ResType:  "ValueSet",
			URIs:     map[string]string{"url": baseURL},
			Count:    1,
		}); err == nil && sr != nil && len(sr.Matches) > 0 {
			hasStoredVS = true
		}
	}
	if inlineVS == nil && !hasExplicitIGCS && !hasStoredVS {
		return false, false
	}

	_, _, concepts, err := s.resolveValueSetConcepts(context.Background(), "default", "", vsCanonical, inlineVS)
	if err != nil || len(concepts) == 0 {
		return false, false
	}
	for _, c := range concepts {
		if c.Code == code && (system == "" || c.System == "" || strings.EqualFold(c.System, system)) {
			return true, true
		}
	}
	return true, false
}

func isAdminIGWriteAllowed(secCtx *auth.SecurityContext) bool {
	if auth.IsPatientRestricted(secCtx) {
		return false
	}
	return auth.Allows(secCtx, "ImplementationGuide", auth.ActionCreate) ||
		auth.Allows(secCtx, "ImplementationGuide", auth.ActionUpdate) ||
		auth.Allows(secCtx, "*", auth.ActionCreate) ||
		auth.Allows(secCtx, "*", auth.ActionUpdate)
}

func isAdminIGReadAllowed(secCtx *auth.SecurityContext) bool {
	if auth.IsPatientRestricted(secCtx) {
		return false
	}
	return auth.Allows(secCtx, "ImplementationGuide", auth.ActionRead) ||
		auth.Allows(secCtx, "ImplementationGuide", auth.ActionSearch) ||
		auth.Allows(secCtx, "CapabilityStatement", auth.ActionRead) ||
		auth.Allows(secCtx, "*", auth.ActionRead) ||
		auth.Allows(secCtx, "*", auth.ActionSearch)
}

// handleInstallIG handles POST /fhir/r4/$install-ig, POST /fhir/r4/{tenant}/$install-ig,
// POST /fhir/r4/$load-ig, and POST /fhir/r4/{tenant}/$load-ig.
func (s *Server) handleInstallIG(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOperationOutcome(w, http.StatusMethodNotAllowed, "not-supported", "$install-ig only supports HTTP POST")
		return
	}
	if secCtx, hasSec := auth.FromContext(r.Context()); hasSec {
		if !isAdminIGWriteAllowed(secCtx) {
			writeOperationOutcome(w, http.StatusForbidden, "forbidden",
				"insufficient SMART scope for $install-ig (requires system or user ImplementationGuide write scope)")
			return
		}
	}

	tenant := strings.TrimSpace(r.PathValue("tenant"))
	if tenant == "" || strings.HasPrefix(tenant, "$") {
		tenant = "default"
	}

	q := r.URL.Query()
	opts := InstallIGOptions{
		Tenant:       tenant,
		Persist:      s.store != nil,
		LoadExamples: strings.EqualFold(strings.TrimSpace(q.Get("loadExamples")), "true"),
		Reindex:      strings.EqualFold(strings.TrimSpace(q.Get("reindex")), "true"),
	}
	if pv := strings.TrimSpace(q.Get("persist")); pv != "" {
		opts.Persist = strings.EqualFold(pv, "true") && s.store != nil
	}

	const maxRequestBytes = 132 << 20 // 132 MiB ceiling for compressed .tgz or base64 JSON wrapper
	if r.Body == nil {
		writeOperationOutcome(w, http.StatusBadRequest, "required", "$install-ig requires a .tgz archive, Binary, or Parameters request body")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes+1))
	if err != nil {
		writeOperationOutcome(w, http.StatusBadRequest, "invalid", "failed to read $install-ig request body")
		return
	}
	if int64(len(body)) > maxRequestBytes {
		writeOperationOutcome(w, http.StatusRequestEntityTooLarge, "too-costly", "$install-ig payload exceeds maximum size limit")
		return
	}
	if len(bytes.TrimSpace(body)) == 0 {
		writeOperationOutcome(w, http.StatusBadRequest, "required", "$install-ig request body is empty")
		return
	}

	ct := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))
	isDirectGzip := len(body) >= 2 && body[0] == 0x1f && body[1] == 0x8b
	if isDirectGzip || strings.Contains(ct, "gzip") || strings.Contains(ct, "octet-stream") || strings.Contains(ct, "tar") {
		pkg, err := igloader.LoadTGZBytes(body, igloader.Options{LoadExamples: opts.LoadExamples})
		if err != nil {
			writeOperationOutcome(w, igErrorHTTPStatus(err), "invalid", err.Error())
			return
		}
		summary, err := s.InstallIGPackage(r.Context(), pkg, opts)
		if err != nil {
			writeOperationOutcome(w, http.StatusInternalServerError, "exception", err.Error())
			return
		}
		s.writeOperationResponse(w, r, http.StatusOK, "Parameters", buildInstallIGParametersResponse([]*InstalledIGSummary{summary}))
		return
	}

	root, err := s.decodeOperationBodyMap(r, body, "Parameters")
	if err != nil {
		writeOperationOutcome(w, http.StatusBadRequest, "invalid", fmt.Sprintf("invalid $install-ig payload: %v", err))
		return
	}

	rt, _ := root["resourceType"].(string)
	var rawArchives [][]byte
	var localPaths []string

	switch rt {
	case "Binary":
		rawB64, _ := root["data"].(string)
		decoded, decErr := decodeBase64Payload(rawB64)
		if decErr != nil || len(decoded) == 0 {
			writeOperationOutcome(w, http.StatusBadRequest, "invalid", "Binary resource must include valid base64 'data' containing an NPM .tgz archive")
			return
		}
		rawArchives = append(rawArchives, decoded)

	case "Parameters":
		paramList, _ := root["parameter"].([]any)
		for _, rawP := range paramList {
			pm, ok := rawP.(map[string]any)
			if !ok {
				continue
			}
			name, _ := pm["name"].(string)
			switch name {
			case "loadExamples":
				if bVal, ok := pm["valueBoolean"].(bool); ok {
					opts.LoadExamples = bVal
				}
			case "reindex":
				if bVal, ok := pm["valueBoolean"].(bool); ok {
					opts.Reindex = bVal
				}
			case "persist":
				if bVal, ok := pm["valueBoolean"].(bool); ok {
					opts.Persist = bVal && s.store != nil
				}
			case "npmPackage", "package", "tgz", "content":
				if b64, ok := pm["valueBase64Binary"].(string); ok && strings.TrimSpace(b64) != "" {
					decoded, decErr := decodeBase64Payload(b64)
					if decErr != nil {
						writeOperationOutcome(w, http.StatusBadRequest, "invalid", fmt.Sprintf("invalid base64 in %q parameter: %v", name, decErr))
						return
					}
					rawArchives = append(rawArchives, decoded)
				} else if att, ok := pm["valueAttachment"].(map[string]any); ok {
					b64, _ := att["data"].(string)
					decoded, decErr := decodeBase64Payload(b64)
					if decErr != nil || len(decoded) == 0 {
						writeOperationOutcome(w, http.StatusBadRequest, "invalid", fmt.Sprintf("invalid valueAttachment.data in %q parameter", name))
						return
					}
					rawArchives = append(rawArchives, decoded)
				} else if resMap, ok := pm["resource"].(map[string]any); ok && resMap["resourceType"] == "Binary" {
					b64, _ := resMap["data"].(string)
					decoded, decErr := decodeBase64Payload(b64)
					if decErr != nil || len(decoded) == 0 {
						writeOperationOutcome(w, http.StatusBadRequest, "invalid", fmt.Sprintf("invalid Binary.data in %q parameter", name))
						return
					}
					rawArchives = append(rawArchives, decoded)
				}
			case "path":
				if pStr := strings.TrimSpace(extractParamString(pm)); pStr != "" {
					s.igMu.RLock()
					allowLocal := s.allowLocalIGPaths
					s.igMu.RUnlock()
					if !allowLocal {
						writeOperationOutcome(w, http.StatusForbidden, "forbidden",
							"server-local filesystem 'path' loading is disabled over HTTP; upload a .tgz archive via npmPackage (valueBase64Binary) or binary POST body")
						return
					}
					localPaths = append(localPaths, pStr)
				}
			}
		}

	default:
		writeOperationOutcome(w, http.StatusBadRequest, "invalid",
			fmt.Sprintf("$install-ig expects a gzipped .tgz body, FHIR Parameters, or FHIR Binary resource (got %q)", rt))
		return
	}

	if len(rawArchives) == 0 && len(localPaths) == 0 {
		writeOperationOutcome(w, http.StatusBadRequest, "required",
			"$install-ig Parameters must provide at least one 'npmPackage' (valueBase64Binary / Binary) parameter")
		return
	}

	var pkgs []*igloader.Package
	for _, rawTGZ := range rawArchives {
		pkg, err := igloader.LoadTGZBytes(rawTGZ, igloader.Options{LoadExamples: opts.LoadExamples})
		if err != nil {
			writeOperationOutcome(w, igErrorHTTPStatus(err), "invalid", err.Error())
			return
		}
		pkgs = append(pkgs, pkg)
	}
	if len(localPaths) > 0 {
		loadedFromDisk, err := igloader.LoadPaths(localPaths, igloader.Options{LoadExamples: opts.LoadExamples})
		if err != nil {
			writeOperationOutcome(w, igErrorHTTPStatus(err), "invalid", err.Error())
			return
		}
		pkgs = append(pkgs, loadedFromDisk...)
	}

	summaries, err := s.InstallIGPackages(r.Context(), pkgs, opts)
	if err != nil {
		writeOperationOutcome(w, igErrorHTTPStatus(err), "exception", err.Error())
		return
	}

	s.writeOperationResponse(w, r, http.StatusOK, "Parameters", buildInstallIGParametersResponse(summaries))
}

// handleListIGs handles GET /fhir/r4/$igs and GET /fhir/r4/{tenant}/$igs.
func (s *Server) handleListIGs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeOperationOutcome(w, http.StatusMethodNotAllowed, "not-supported", "$igs only supports HTTP GET")
		return
	}
	if secCtx, hasSec := auth.FromContext(r.Context()); hasSec {
		if !isAdminIGReadAllowed(secCtx) {
			writeOperationOutcome(w, http.StatusForbidden, "forbidden",
				"insufficient SMART scope for $igs")
			return
		}
	}

	installed := s.InstalledIGs()
	params := []map[string]any{
		{"name": "count", "valueInteger": len(installed)},
	}
	for _, ig := range installed {
		params = append(params, buildIGSummaryParameterPart(ig))
	}

	s.writeOperationResponse(w, r, http.StatusOK, "Parameters", map[string]any{
		"resourceType": "Parameters",
		"parameter":    params,
	})
}

func buildInstallIGParametersResponse(summaries []*InstalledIGSummary) map[string]any {
	totalPersisted := 0
	totalReindexed := 0
	for _, sum := range summaries {
		totalPersisted += sum.PersistedResources
		totalReindexed += sum.ReindexedResources
	}
	params := []map[string]any{
		{"name": "installedCount", "valueInteger": len(summaries)},
		{"name": "persistedResources", "valueInteger": totalPersisted},
		{"name": "reindexedResources", "valueInteger": totalReindexed},
	}
	for _, sum := range summaries {
		params = append(params, buildIGSummaryParameterPart(sum))
	}
	return map[string]any{
		"resourceType": "Parameters",
		"parameter":    params,
	}
}

func buildIGSummaryParameterPart(sum *InstalledIGSummary) map[string]any {
	parts := []map[string]any{
		{"name": "name", "valueString": sum.Name},
		{"name": "version", "valueString": sum.Version},
		{"name": "conformanceCount", "valueInteger": sum.TotalConformance},
		{"name": "exampleCount", "valueInteger": sum.TotalExamples},
		{"name": "persistedResources", "valueInteger": sum.PersistedResources},
		{"name": "reindexedResources", "valueInteger": sum.ReindexedResources},
	}
	if sum.Canonical != "" {
		parts = append(parts, map[string]any{"name": "canonical", "valueCanonical": sum.Canonical})
	}
	if sum.Title != "" {
		parts = append(parts, map[string]any{"name": "title", "valueString": sum.Title})
	}
	if !sum.LoadedAt.IsZero() {
		parts = append(parts, map[string]any{"name": "loadedAt", "valueDateTime": sum.LoadedAt.UTC().Format(time.RFC3339)})
	}

	var rtKeys []string
	for rt := range sum.ArtifactCounts {
		rtKeys = append(rtKeys, rt)
	}
	sort.Strings(rtKeys)
	for _, rt := range rtKeys {
		parts = append(parts, map[string]any{
			"name": "artifactCount",
			"part": []map[string]any{
				{"name": "resourceType", "valueCode": rt},
				{"name": "count", "valueInteger": sum.ArtifactCounts[rt]},
			},
		})
	}

	return map[string]any{
		"name": "package",
		"part": parts,
	}
}

func decodeBase64Payload(raw string) ([]byte, error) {
	cleaned := strings.Map(func(r rune) rune {
		if r == ' ' || r == '\n' || r == '\r' || r == '\t' {
			return -1
		}
		return r
	}, raw)
	if cleaned == "" {
		return nil, nil
	}
	return base64.StdEncoding.DecodeString(cleaned)
}

func igErrorHTTPStatus(err error) int {
	if err == nil {
		return http.StatusOK
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "exceeds maximum"):
		return http.StatusRequestEntityTooLarge
	case strings.Contains(msg, "incompatible with R4"),
		strings.Contains(msg, "circular package dependency"):
		return http.StatusUnprocessableEntity
	default:
		return http.StatusBadRequest
	}
}
