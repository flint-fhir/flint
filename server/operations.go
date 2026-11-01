package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/flint-fhir/flint/pkg/auth"
	automq "github.com/flint-fhir/flint/pkg/cdc"
	"github.com/flint-fhir/flint/pkg/fhirutil"
	"github.com/flint-fhir/flint/store/postgres"
)

type patientDemographics struct {
	identifiers []codingKey
	families    []string
	givens      []string
	birthDate   string
	gender      string
	postalCodes []string
	telecoms    []string
}

type codingKey struct {
	system string
	code   string
}

func extractPatientDemographics(pat map[string]any) patientDemographics {
	var d patientDemographics
	if rawIDs, ok := pat["identifier"].([]any); ok {
		for _, item := range rawIDs {
			if m, ok := item.(map[string]any); ok {
				sys := strings.ToLower(strings.TrimSpace(stringField(m, "system")))
				val := strings.ToLower(strings.TrimSpace(stringField(m, "value")))
				if val != "" {
					d.identifiers = append(d.identifiers, codingKey{system: sys, code: val})
				}
			}
		}
	}
	if rawNames, ok := pat["name"].([]any); ok {
		for _, item := range rawNames {
			if m, ok := item.(map[string]any); ok {
				if fam := strings.ToLower(strings.TrimSpace(stringField(m, "family"))); fam != "" {
					d.families = append(d.families, fam)
				}
				if givs, ok := m["given"].([]any); ok {
					for _, g := range givs {
						if gs, ok := g.(string); ok {
							if trimmed := strings.ToLower(strings.TrimSpace(gs)); trimmed != "" {
								d.givens = append(d.givens, trimmed)
							}
						}
					}
				}
			}
		}
	}
	d.birthDate = strings.TrimSpace(stringField(pat, "birthDate"))
	d.gender = strings.ToLower(strings.TrimSpace(stringField(pat, "gender")))
	if rawAddrs, ok := pat["address"].([]any); ok {
		for _, item := range rawAddrs {
			if m, ok := item.(map[string]any); ok {
				if pc := strings.ToLower(strings.TrimSpace(stringField(m, "postalCode"))); pc != "" {
					d.postalCodes = append(d.postalCodes, pc)
				}
			}
		}
	}
	if rawTel, ok := pat["telecom"].([]any); ok {
		for _, item := range rawTel {
			if m, ok := item.(map[string]any); ok {
				if v := normalizeTelecomValue(stringField(m, "value")); v != "" {
					d.telecoms = append(d.telecoms, v)
				}
			}
		}
	}
	return d
}

func (d patientDemographics) isEmpty() bool {
	return len(d.identifiers) == 0 &&
		len(d.families) == 0 &&
		len(d.givens) == 0 &&
		d.birthDate == "" &&
		d.gender == "" &&
		len(d.postalCodes) == 0 &&
		len(d.telecoms) == 0
}

func normalizeTelecomValue(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if strings.Contains(s, "@") {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func stringField(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if s, ok := m[key].(string); ok {
		return s
	}
	return ""
}

func scorePatientCandidate(target, cand patientDemographics) float64 {
	score := 0.0

	// 1. Identifier match (+0.50)
	idMatched := false
	for _, tid := range target.identifiers {
		for _, cid := range cand.identifiers {
			if tid.code == cid.code && (tid.system == "" || cid.system == "" || tid.system == cid.system) {
				idMatched = true
				break
			}
		}
		if idMatched {
			break
		}
	}
	if idMatched {
		score += 0.50
	}

	// 2. Family name match (+0.15)
	famMatched := false
	for _, tf := range target.families {
		for _, cf := range cand.families {
			if tf == cf {
				famMatched = true
				break
			}
		}
		if famMatched {
			break
		}
	}
	if famMatched {
		score += 0.15
	}

	// 3. Given name match (+0.10 exact, +0.05 prefix/initial)
	givenScore := 0.0
	for _, tg := range target.givens {
		for _, cg := range cand.givens {
			if tg == cg {
				givenScore = 0.10
				break
			}
			if strings.HasPrefix(tg, cg) || strings.HasPrefix(cg, tg) {
				if givenScore < 0.05 {
					givenScore = 0.05
				}
			}
		}
		if givenScore == 0.10 {
			break
		}
	}
	score += givenScore

	// 4. BirthDate match (+0.15 exact, -0.10 mismatch if both present)
	if target.birthDate != "" && cand.birthDate != "" {
		if target.birthDate == cand.birthDate {
			score += 0.15
		}
	}

	// 5. Gender match (+0.05)
	if target.gender != "" && cand.gender != "" && target.gender == cand.gender {
		score += 0.05
	}

	// 6. PostalCode or Telecom match (+0.05)
	contactMatched := false
	for _, tpc := range target.postalCodes {
		for _, cpc := range cand.postalCodes {
			if tpc == cpc {
				contactMatched = true
				break
			}
		}
	}
	if !contactMatched {
		for _, tt := range target.telecoms {
			for _, ct := range cand.telecoms {
				if tt != "" && tt == ct {
					contactMatched = true
					break
				}
			}
		}
	}
	if contactMatched {
		score += 0.05
	}

	// Bonus when deterministic demographics (family + given + birthDate) all align
	// even if MRN/identifier was not supplied in the query template:
	if !idMatched && famMatched && givenScore == 0.10 && target.birthDate != "" && target.birthDate == cand.birthDate {
		score += 0.40
	}

	if score > 0.99 {
		score = 0.99
	}
	return math.Round(score*100) / 100
}

func matchGradeForScore(score float64) string {
	switch {
	case score >= 0.80:
		return "certain"
	case score >= 0.50:
		return "probable"
	default:
		return "possible"
	}
}

// handlePatientMatch implements POST /fhir/r4/{tenant}/Patient/$match
// per HL7 FHIR R4 OperationDefinition/Patient-match (MPI probabilistic/deterministic matching).
func (s *Server) handlePatientMatch(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	if s.store == nil {
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "operational store is not configured")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 5*1024*1024))
	if err != nil || len(body) == 0 {
		writeOperationOutcome(w, http.StatusBadRequest, "invalid", "Patient/$match requires a non-empty Parameters JSON body")
		return
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		writeOperationOutcome(w, http.StatusBadRequest, "structure", "invalid JSON in Patient/$match body")
		return
	}

	var templatePatient map[string]any
	onlyCertain := false
	maxCount := 20

	switch payload["resourceType"] {
	case "Parameters":
		if params, ok := payload["parameter"].([]any); ok {
			for _, rawParam := range params {
				pm, ok := rawParam.(map[string]any)
				if !ok {
					continue
				}
				switch pm["name"] {
				case "resource":
					if res, ok := pm["resource"].(map[string]any); ok {
						templatePatient = res
					}
				case "onlyCertainMatches":
					if b, ok := pm["valueBoolean"].(bool); ok {
						onlyCertain = b
					}
				case "count":
					if n, ok := pm["valueInteger"].(float64); ok && int(n) > 0 {
						maxCount = int(n)
					}
				}
			}
		}
	case "Patient":
		templatePatient = payload
	default:
		writeOperationOutcome(w, http.StatusBadRequest, "invalid", "Patient/$match body must be a FHIR Parameters resource")
		return
	}

	if templatePatient == nil || templatePatient["resourceType"] != "Patient" {
		writeOperationOutcome(w, http.StatusBadRequest, "required", "Patient/$match requires a parameter named 'resource' containing a Patient resource")
		return
	}

	targetDemo := extractPatientDemographics(templatePatient)
	if targetDemo.isEmpty() {
		writeOperationOutcome(w, http.StatusBadRequest, "required", "Patient/$match template Patient must contain at least one demographic attribute (identifier, name, birthDate, gender, address, or telecom)")
		return
	}

	var candIdentifiers []string
	for _, idKey := range targetDemo.identifiers {
		if idKey.code != "" {
			candIdentifiers = append(candIdentifiers, idKey.code)
		}
	}
	candParams := postgres.PatientMatchCandidateParams{
		TenantID:    tenant,
		Identifiers: candIdentifiers,
		Families:    targetDemo.families,
		Givens:      targetDemo.givens,
		BirthDate:   targetDemo.birthDate,
		Limit:       500,
	}
	if secCtx, ok := auth.FromContext(r.Context()); ok && auth.IsPatientRestricted(secCtx) {
		candParams.RestrictedPatientID = cleanID(secCtx.GetPatientId())
	}

	candidates, err := s.store.SearchPatientMatchCandidates(r.Context(), candParams)
	if err != nil {
		s.logger.Error("patient $match search failed", "error", err, "tenant", tenant)
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to evaluate Patient/$match candidates")
		return
	}

	type scoredCandidate struct {
		id       string
		score    float64
		grade    string
		resource map[string]any
	}
	var matches []scoredCandidate

	secCtx, hasSecCtx := auth.FromContext(r.Context())
	for _, rec := range candidates {
		if hasSecCtx && !s.resourceAllowedByGranularScope(secCtx, tenant, "Patient", rec.ResID, rec.ResourceProto) {
			continue
		}
		candMap, err := s.protoToFHIRMapWithMeta("Patient", rec.ResourceProto, rec.ResVersion, rec.LastUpdated)
		if err != nil {
			continue
		}
		candDemo := extractPatientDemographics(candMap)
		score := scorePatientCandidate(targetDemo, candDemo)
		if score < 0.20 {
			continue
		}
		if onlyCertain && score < 0.80 {
			continue
		}
		matches = append(matches, scoredCandidate{
			id:       rec.ResID,
			score:    score,
			grade:    matchGradeForScore(score),
			resource: candMap,
		})
	}

	sort.Slice(matches, func(i, j int) bool {
		if matches[i].score != matches[j].score {
			return matches[i].score > matches[j].score
		}
		return matches[i].id < matches[j].id
	})

	if len(matches) > maxCount {
		matches = matches[:maxCount]
	}

	entries := make([]map[string]any, 0, len(matches))
	for _, m := range matches {
		entries = append(entries, map[string]any{
			"fullUrl":  "Patient/" + m.id,
			"resource": m.resource,
			"search": map[string]any{
				"mode":  "match",
				"score": m.score,
				"extension": []map[string]any{
					{
						"url":       "http://hl7.org/fhir/StructureDefinition/match-grade",
						"valueCode": m.grade,
					},
				},
			},
		})
	}

	bundle := map[string]any{
		"resourceType": "Bundle",
		"type":         "searchset",
		"total":        len(entries),
		"entry":        entries,
	}

	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(bundle)
}

// handleMeta implements GET /{tenant}/$meta, GET /{tenant}/{resourceType}/$meta,
// and GET /{tenant}/{resourceType}/{id}/$meta per HL7 FHIR R4 OperationDefinition/Resource-meta.
func (s *Server) handleMeta(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	resType := r.PathValue("resourceType")
	resID := r.PathValue("id")

	if s.store == nil {
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "operational store is not configured")
		return
	}

	// 1. Instance-level: GET /{tenant}/{resourceType}/{id}/$meta
	if resType != "" && resID != "" {
		rec, err := s.store.ReadResourceWithMeta(r.Context(), tenant, resType, resID)
		if errors.Is(err, sql.ErrNoRows) {
			writeOperationOutcome(w, http.StatusNotFound, "not-found", fmt.Sprintf("%s/%s not found", resType, resID))
			return
		}
		if errors.Is(err, postgres.ErrResourceDeleted) {
			writeOperationOutcome(w, http.StatusGone, "deleted", fmt.Sprintf("%s/%s has been deleted", resType, resID))
			return
		}
		if err != nil {
			writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to read resource meta")
			return
		}
		if secCtx, hasSecCtx := auth.FromContext(r.Context()); hasSecCtx && !s.resourceAllowedByGranularScope(secCtx, tenant, resType, resID, rec.ResourceProto) {
			writeOperationOutcome(w, http.StatusForbidden, "forbidden",
				fmt.Sprintf("insufficient granular scope to read %s/%s $meta", resType, resID))
			return
		}
		resMap, err := s.protoToFHIRMapWithMeta(resType, rec.ResourceProto, rec.ResVersion, rec.LastUpdated)
		if err != nil {
			writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to serialize resource meta")
			return
		}
		metaObj, _ := resMap["meta"].(map[string]any)
		if metaObj == nil {
			metaObj = make(map[string]any)
		}
		writeMetaParametersResponse(w, metaObj)
		return
	}

	// 2. Type-level or System-level $meta: aggregate distinct profiles, security labels, and tags
	var targetTypes []string
	if resType != "" {
		if _, ok := s.protoRegistry[resType]; !ok {
			writeOperationOutcome(w, http.StatusBadRequest, "not-supported", fmt.Sprintf("resource type %s not registered", resType))
			return
		}
		targetTypes = []string{resType}
	} else {
		targetTypes = fhirutil.USCoreResourceTypes()
	}

	seenProfiles := make(map[string]bool)
	var profiles []string
	seenSecurity := make(map[string]bool)
	var securities []map[string]any
	seenTags := make(map[string]bool)
	var tags []map[string]any

	secCtx, hasSecCtx := auth.FromContext(r.Context())
	patientRestricted := hasSecCtx && auth.IsPatientRestricted(secCtx)
	allowedPatientID := ""
	if patientRestricted {
		allowedPatientID = cleanID(secCtx.GetPatientId())
	}

	for _, rt := range targetTypes {
		if hasSecCtx {
			if !auth.Allows(secCtx, rt, auth.ActionRead) && !auth.Allows(secCtx, rt, auth.ActionSearch) {
				continue
			}
			if patientRestricted && !strings.EqualFold(rt, "Patient") && !postgres.IsPatientCompartmentResourceType(rt) {
				continue
			}
		}
		for _, prof := range usCoreSupportedProfiles(rt) {
			if !seenProfiles[prof] {
				seenProfiles[prof] = true
				profiles = append(profiles, prof)
			}
		}
		sp := postgres.SearchParams{
			TenantID: tenant,
			ResType:  rt,
			Count:    100,
		}
		if patientRestricted && allowedPatientID != "" {
			if strings.EqualFold(rt, "Patient") {
				sp.ID = allowedPatientID
			} else if postgres.IsPatientCompartmentResourceType(rt) {
				sp.References = map[string]string{"patient": "Patient/" + allowedPatientID}
				sp.ReferenceFilters = map[string][]string{"patient": {"Patient/" + allowedPatientID}}
			}
		}
		sr, err := s.store.Search(r.Context(), sp)
		if err != nil {
			continue
		}
		for _, rec := range sr.Matches {
			if hasSecCtx && !s.resourceAllowedByGranularScope(secCtx, tenant, rt, rec.ResID, rec.ResourceProto) {
				continue
			}
			resMap, err := s.protoToFHIRMapWithMeta(rt, rec.ResourceProto, rec.ResVersion, rec.LastUpdated)
			if err != nil {
				continue
			}
			metaMap, _ := resMap["meta"].(map[string]any)
			if metaMap == nil {
				continue
			}
			if rawProfs, ok := metaMap["profile"].([]any); ok {
				for _, p := range rawProfs {
					if ps, ok := p.(string); ok && ps != "" && !seenProfiles[ps] {
						seenProfiles[ps] = true
						profiles = append(profiles, ps)
					}
				}
			}
			if rawSecs, ok := metaMap["security"].([]any); ok {
				for _, sec := range rawSecs {
					if sm, ok := sec.(map[string]any); ok {
						key := codingDedupeKey(sm)
						if key != "|" && !seenSecurity[key] {
							seenSecurity[key] = true
							securities = append(securities, sm)
						}
					}
				}
			}
			if rawTags, ok := metaMap["tag"].([]any); ok {
				for _, tg := range rawTags {
					if tm, ok := tg.(map[string]any); ok {
						key := codingDedupeKey(tm)
						if key != "|" && !seenTags[key] {
							seenTags[key] = true
							tags = append(tags, tm)
						}
					}
				}
			}
		}
	}

	if !hasSecCtx && s.store != nil {
		if agg, err := s.store.AggregateMeta(r.Context(), tenant, resType); err == nil && agg != nil {
			for _, p := range agg.Profiles {
				if p != "" && !seenProfiles[p] {
					seenProfiles[p] = true
					profiles = append(profiles, p)
				}
			}
			for _, sec := range agg.Security {
				key := sec.System + "|" + sec.Code
				if key != "|" && !seenSecurity[key] {
					seenSecurity[key] = true
					m := map[string]any{"code": sec.Code}
					if sec.System != "" {
						m["system"] = sec.System
					}
					securities = append(securities, m)
				}
			}
			for _, tg := range agg.Tags {
				key := tg.System + "|" + tg.Code
				if key != "|" && !seenTags[key] {
					seenTags[key] = true
					m := map[string]any{"code": tg.Code}
					if tg.System != "" {
						m["system"] = tg.System
					}
					tags = append(tags, m)
				}
			}
		}
	}

	metaOut := make(map[string]any)
	if len(profiles) > 0 {
		metaOut["profile"] = profiles
	}
	if len(securities) > 0 {
		metaOut["security"] = securities
	}
	if len(tags) > 0 {
		metaOut["tag"] = tags
	}
	writeMetaParametersResponse(w, metaOut)
}

// handleMetaAdd implements POST /fhir/r4/{tenant}/{resourceType}/{id}/$meta-add.
func (s *Server) handleMetaAdd(w http.ResponseWriter, r *http.Request) {
	s.mutateInstanceMeta(w, r, false)
}

// handleMetaDelete implements POST /fhir/r4/{tenant}/{resourceType}/{id}/$meta-delete.
func (s *Server) handleMetaDelete(w http.ResponseWriter, r *http.Request) {
	s.mutateInstanceMeta(w, r, true)
}

func (s *Server) mutateInstanceMeta(w http.ResponseWriter, r *http.Request, isDelete bool) {
	tenant := r.PathValue("tenant")
	resType := r.PathValue("resourceType")
	resID := r.PathValue("id")

	md, ok := s.protoRegistry[resType]
	if !ok {
		writeOperationOutcome(w, http.StatusBadRequest, "not-supported", fmt.Sprintf("resource type %s not registered", resType))
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 2*1024*1024))
	if err != nil || len(body) == 0 {
		writeOperationOutcome(w, http.StatusBadRequest, "invalid", "non-empty Parameters body required for $meta operation")
		return
	}

	deltaMeta, err := extractInputValueMeta(body)
	if err != nil {
		writeOperationOutcome(w, http.StatusBadRequest, "required", err.Error())
		return
	}

	rec, err := s.store.ReadResourceWithMeta(r.Context(), tenant, resType, resID)
	if errors.Is(err, sql.ErrNoRows) {
		writeOperationOutcome(w, http.StatusNotFound, "not-found", fmt.Sprintf("%s/%s not found", resType, resID))
		return
	}
	if errors.Is(err, postgres.ErrResourceDeleted) {
		writeOperationOutcome(w, http.StatusGone, "deleted", fmt.Sprintf("%s/%s has been deleted", resType, resID))
		return
	}
	if err != nil {
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to read resource")
		return
	}
	if secCtx, hasSecCtx := auth.FromContext(r.Context()); hasSecCtx {
		if constraints, constrained := auth.ScopeQueryConstraints(secCtx, resType, auth.ActionUpdate); constrained {
			if !s.resourceSatisfiesScopeConstraints(tenant, resType, resID, rec.ResourceProto, constraints) {
				writeOperationOutcome(w, http.StatusForbidden, "forbidden",
					fmt.Sprintf("insufficient granular SMART v2 scope to mutate meta of %s/%s", resType, resID))
				return
			}
		}
	}

	resMap, err := s.protoToFHIRMapWithMeta(resType, rec.ResourceProto, rec.ResVersion, rec.LastUpdated)
	if err != nil {
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to deserialize current resource")
		return
	}

	curMeta, _ := resMap["meta"].(map[string]any)
	if curMeta == nil {
		curMeta = make(map[string]any)
	}

	if isDelete {
		applyMetaDelete(curMeta, deltaMeta)
	} else {
		applyMetaAdd(curMeta, deltaMeta)
	}
	resMap["meta"] = curMeta

	updatedRawJSON, err := json.Marshal(resMap)
	if err != nil {
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to marshal updated resource")
		return
	}

	normalizedBody, err := fhirutil.NormalizeFHIRJSONToProtoJSON(md, updatedRawJSON)
	if err != nil {
		writeOperationOutcome(w, http.StatusBadRequest, "structure", fmt.Sprintf("invalid meta update: %v", err))
		return
	}

	msg := dynamicpb.NewMessage(md)
	unmarshaler := protojson.UnmarshalOptions{DiscardUnknown: true}
	if err := unmarshaler.Unmarshal(normalizedBody, msg); err != nil {
		writeOperationOutcome(w, http.StatusBadRequest, "structure", fmt.Sprintf("invalid meta proto: %v", err))
		return
	}

	protoBytes, err := proto.Marshal(msg)
	if err != nil {
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to marshal updated proto")
		return
	}

	var idx *postgres.SearchIndexes
	if extractor, hasExtractor := s.extractors[resType]; hasExtractor {
		idx, _ = extractor(tenant, resID, protoBytes)
	}
	if strings.EqualFold(resType, "Provenance") {
		s.enrichProvenancePatientIndex(r.Context(), tenant, resID, idx)
	}

	newRec, err := s.store.WriteResourceWithMeta(r.Context(), postgres.ResourceInput{
		TenantID:      tenant,
		ResType:       resType,
		ResID:         resID,
		ResourceProto: protoBytes,
		SearchIndexes: idx,
	})
	if err != nil {
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to persist updated resource meta")
		return
	}

	s.publishCDC(r.Context(), automq.Message{
		TenantID:  tenant,
		ResType:   resType,
		ResID:     resID,
		ProtoData: protoBytes,
		Op:        "U",
	})

	finalMap, err := s.protoToFHIRMapWithMeta(resType, protoBytes, newRec.ResVersion, newRec.LastUpdated)
	if err != nil {
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to serialize updated resource meta")
		return
	}
	finalMeta, _ := finalMap["meta"].(map[string]any)
	if finalMeta == nil {
		finalMeta = curMeta
	}

	w.Header().Set("ETag", postgres.FormatETag(newRec.ResVersion))
	w.Header().Set("Last-Modified", newRec.LastUpdated.UTC().Format(http.TimeFormat))
	writeMetaParametersResponse(w, finalMeta)
}

func extractInputValueMeta(body []byte) (map[string]any, error) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("invalid JSON body: %w", err)
	}
	if payload["resourceType"] == "Parameters" {
		if params, ok := payload["parameter"].([]any); ok {
			for _, rawParam := range params {
				if pm, ok := rawParam.(map[string]any); ok && pm["name"] == "meta" {
					if vm, ok := pm["valueMeta"].(map[string]any); ok {
						return vm, nil
					}
				}
			}
		}
		return nil, fmt.Errorf("parameters resource must contain a parameter with name='meta' and a valueMeta object")
	}
	if vm, ok := payload["meta"].(map[string]any); ok {
		return vm, nil
	}
	return nil, fmt.Errorf("request body must be a FHIR Parameters resource with valueMeta")
}

func applyMetaAdd(curMeta, addMeta map[string]any) {
	// 1. Merge profile array
	if addProfs, ok := addMeta["profile"].([]any); ok && len(addProfs) > 0 {
		existing, _ := curMeta["profile"].([]any)
		seen := make(map[string]bool, len(existing))
		for _, p := range existing {
			if ps, ok := p.(string); ok {
				seen[ps] = true
			}
		}
		for _, p := range addProfs {
			if ps, ok := p.(string); ok && ps != "" && !seen[ps] {
				seen[ps] = true
				existing = append(existing, ps)
			}
		}
		curMeta["profile"] = existing
	}

	// 2. Merge security array
	if addSec, ok := addMeta["security"].([]any); ok && len(addSec) > 0 {
		curMeta["security"] = mergeCodingSlices(curMeta["security"], addSec)
	}

	// 3. Merge tag array
	if addTag, ok := addMeta["tag"].([]any); ok && len(addTag) > 0 {
		curMeta["tag"] = mergeCodingSlices(curMeta["tag"], addTag)
	}
}

func applyMetaDelete(curMeta, delMeta map[string]any) {
	// 1. Remove matching profile URIs
	if delProfs, ok := delMeta["profile"].([]any); ok && len(delProfs) > 0 {
		removeSet := make(map[string]bool, len(delProfs))
		for _, p := range delProfs {
			if ps, ok := p.(string); ok && ps != "" {
				removeSet[ps] = true
			}
		}
		if existing, ok := curMeta["profile"].([]any); ok {
			kept := make([]any, 0, len(existing))
			for _, p := range existing {
				if ps, ok := p.(string); ok && !removeSet[ps] {
					kept = append(kept, ps)
				}
			}
			if len(kept) > 0 {
				curMeta["profile"] = kept
			} else {
				delete(curMeta, "profile")
			}
		}
	}

	// 2. Remove matching security codings
	if delSec, ok := delMeta["security"].([]any); ok && len(delSec) > 0 {
		kept := removeCodingSlices(curMeta["security"], delSec)
		if len(kept) > 0 {
			curMeta["security"] = kept
		} else {
			delete(curMeta, "security")
		}
	}

	// 3. Remove matching tag codings
	if delTag, ok := delMeta["tag"].([]any); ok && len(delTag) > 0 {
		kept := removeCodingSlices(curMeta["tag"], delTag)
		if len(kept) > 0 {
			curMeta["tag"] = kept
		} else {
			delete(curMeta, "tag")
		}
	}
}

func mergeCodingSlices(existingRaw any, additions []any) []any {
	existing, _ := existingRaw.([]any)
	seen := make(map[string]bool, len(existing))
	for _, item := range existing {
		if m, ok := item.(map[string]any); ok {
			seen[codingDedupeKey(m)] = true
		}
	}
	for _, item := range additions {
		if m, ok := item.(map[string]any); ok {
			key := codingDedupeKey(m)
			if key != "|" && !seen[key] {
				seen[key] = true
				existing = append(existing, m)
			}
		}
	}
	return existing
}

func removeCodingSlices(existingRaw any, removals []any) []any {
	existing, _ := existingRaw.([]any)
	if len(existing) == 0 {
		return nil
	}
	removeKeys := make(map[string]bool, len(removals))
	removeCodesOnly := make(map[string]bool, len(removals))
	for _, item := range removals {
		if m, ok := item.(map[string]any); ok {
			sys := strings.TrimSpace(stringField(m, "system"))
			code := strings.TrimSpace(stringField(m, "code"))
			if sys == "" && code != "" {
				removeCodesOnly[code] = true
			} else if code != "" {
				removeKeys[sys+"|"+code] = true
			}
		}
	}
	kept := make([]any, 0, len(existing))
	for _, item := range existing {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		code := strings.TrimSpace(stringField(m, "code"))
		if removeKeys[codingDedupeKey(m)] || removeCodesOnly[code] {
			continue
		}
		kept = append(kept, m)
	}
	return kept
}

func codingDedupeKey(m map[string]any) string {
	return strings.TrimSpace(stringField(m, "system")) + "|" + strings.TrimSpace(stringField(m, "code"))
}

func writeMetaParametersResponse(w http.ResponseWriter, metaObj map[string]any) {
	resp := map[string]any{
		"resourceType": "Parameters",
		"parameter": []map[string]any{
			{
				"name":      "return",
				"valueMeta": metaObj,
			},
		},
	}
	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(resp)
}

type parsedGraphDefinition struct {
	ResourceType string               `json:"resourceType"`
	ID           string               `json:"id,omitempty"`
	URL          string               `json:"url,omitempty"`
	Name         string               `json:"name,omitempty"`
	Status       string               `json:"status,omitempty"`
	Start        string               `json:"start,omitempty"`
	Link         []postgres.GraphLink `json:"link,omitempty"`
}

// handleResourceGraph handles GET/POST /fhir/r4/{tenant}/{resourceType}/{id}/$graph,
// /fhir/r4/{tenant}/GraphDefinition/{id}/$graph, and /fhir/r4/{tenant}/GraphDefinition/$graph
// per HL7 FHIR R4 OperationDefinition/Resource-graph (http://hl7.org/fhir/OperationDefinition/Resource-graph).
func (s *Server) handleResourceGraph(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	resType := strings.TrimSpace(r.PathValue("resourceType"))
	resID := strings.TrimSpace(r.PathValue("id"))

	if _, ok := s.protoRegistry[resType]; !ok {
		writeOperationOutcome(w, http.StatusNotFound, "not-found",
			fmt.Sprintf("unknown or unsupported resource type %q", resType))
		return
	}
	if s.store == nil {
		writeOperationOutcome(w, http.StatusServiceUnavailable, "exception", "store not configured")
		return
	}

	q := r.URL.Query()
	graphRef := strings.TrimSpace(q.Get("graph"))
	if graphRef == "" && strings.EqualFold(resType, "GraphDefinition") {
		graphRef = strings.TrimSpace(q.Get("url"))
	}
	startParam := strings.TrimSpace(q.Get("start"))
	if startParam == "" {
		startParam = strings.TrimSpace(q.Get("resource"))
	}
	if startParam == "" {
		startParam = strings.TrimSpace(q.Get("subject"))
	}
	var inlineGraph *parsedGraphDefinition

	if r.Method == http.MethodPost && r.Body != nil {
		body, err := io.ReadAll(io.LimitReader(r.Body, 2*1024*1024))
		if err != nil {
			writeOperationOutcome(w, http.StatusBadRequest, "invalid", "failed to read request body")
			return
		}
		trimmed := strings.TrimSpace(string(body))
		if trimmed != "" {
			var rootMap map[string]any
			if err := json.Unmarshal([]byte(trimmed), &rootMap); err != nil {
				writeOperationOutcome(w, http.StatusBadRequest, "structure", fmt.Sprintf("invalid JSON body: %v", err))
				return
			}
			rt, _ := rootMap["resourceType"].(string)
			switch rt {
			case "GraphDefinition":
				var gd parsedGraphDefinition
				if err := json.Unmarshal([]byte(trimmed), &gd); err != nil {
					writeOperationOutcome(w, http.StatusBadRequest, "structure", fmt.Sprintf("invalid GraphDefinition JSON: %v", err))
					return
				}
				inlineGraph = &gd
			case "Parameters":
				if params, ok := rootMap["parameter"].([]any); ok {
					for _, p := range params {
						pMap, ok := p.(map[string]any)
						if !ok {
							continue
						}
						name, _ := pMap["name"].(string)
						switch name {
						case "graph", "url", "graphDefinition":
							if resObj, ok := pMap["resource"].(map[string]any); ok {
								if b, err := json.Marshal(resObj); err == nil {
									var gd parsedGraphDefinition
									if json.Unmarshal(b, &gd) == nil && gd.ResourceType == "GraphDefinition" {
										inlineGraph = &gd
									}
								}
							}
							for _, k := range []string{"valueUri", "valueCanonical", "valueString", "valueCode", "valueId"} {
								if v, ok := pMap[k].(string); ok && strings.TrimSpace(v) != "" {
									graphRef = strings.TrimSpace(v)
									break
								}
							}
						case "start", "resource", "subject":
							for _, k := range []string{"valueString", "valueUri", "valueCanonical", "valueId", "valueCode"} {
								if v, ok := pMap[k].(string); ok && strings.TrimSpace(v) != "" && startParam == "" {
									startParam = strings.TrimSpace(v)
									break
								}
							}
							if startParam == "" {
								if refMap, ok := pMap["valueReference"].(map[string]any); ok {
									if refStr, _ := refMap["reference"].(string); strings.TrimSpace(refStr) != "" {
										startParam = strings.TrimSpace(refStr)
									}
								}
							}
						}
					}
				}
			default:
				writeOperationOutcome(w, http.StatusBadRequest, "invalid",
					"expected GraphDefinition or Parameters resource in $graph POST body")
				return
			}
		}
	}

	var startType string
	var links []postgres.GraphLink

	if strings.EqualFold(resType, "GraphDefinition") {
		if graphRef == "" && inlineGraph == nil && resID != "" {
			graphRef = resID
		}
		if graphRef == "" && inlineGraph == nil {
			writeOperationOutcome(w, http.StatusBadRequest, "required",
				"GraphDefinition/$graph requires a GraphDefinition {id}, 'graph' parameter, or inline GraphDefinition")
			return
		}
		if startParam == "" {
			writeOperationOutcome(w, http.StatusBadRequest, "required",
				"GraphDefinition/$graph requires a 'start' resource reference parameter (e.g., start=Patient/123)")
			return
		}

		var targetType, targetID string
		if strings.Contains(startParam, "/") {
			parts := strings.Split(strings.Trim(startParam, "/"), "/")
			if len(parts) >= 2 {
				targetType = parts[len(parts)-2]
				targetID = parts[len(parts)-1]
			}
		} else {
			targetID = startParam
		}

		if inlineGraph != nil {
			startType = strings.TrimSpace(inlineGraph.Start)
			links = inlineGraph.Link
		} else {
			hintType := targetType
			if hintType == "" {
				hintType = "Patient"
			}
			gd, found, err := s.resolveStoredOrBuiltinGraphDefinition(r, tenant, graphRef, hintType)
			if err != nil {
				s.logger.Error("resolve GraphDefinition", "error", err, "tenant", tenant, "graph", graphRef)
				writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to resolve GraphDefinition")
				return
			}
			if !found {
				writeOperationOutcome(w, http.StatusNotFound, "not-found",
					fmt.Sprintf("GraphDefinition %q not found", graphRef))
				return
			}
			startType = strings.TrimSpace(gd.Start)
			links = gd.Link
		}

		if targetType == "" {
			if startType != "" {
				targetType = startType
			} else {
				targetType = "Patient"
			}
		}
		if targetID == "" {
			writeOperationOutcome(w, http.StatusBadRequest, "required",
				"GraphDefinition/$graph 'start' parameter must include a resource ID")
			return
		}
		if _, ok := s.protoRegistry[targetType]; !ok {
			writeOperationOutcome(w, http.StatusNotFound, "not-found",
				fmt.Sprintf("unknown or unsupported start resource type %q", targetType))
			return
		}

		if secCtx, hasSecCtx := auth.FromContext(r.Context()); hasSecCtx && secCtx != nil {
			if !auth.Allows(secCtx, targetType, auth.ActionRead) && !auth.Allows(secCtx, targetType, auth.ActionSearch) {
				writeOperationOutcome(w, http.StatusForbidden, "forbidden",
					fmt.Sprintf("insufficient scope for GraphDefinition/$graph on %s", targetType))
				return
			}
			if auth.IsPatientRestricted(secCtx) && strings.EqualFold(targetType, "Patient") && cleanID(secCtx.GetPatientId()) != cleanID(targetID) {
				writeOperationOutcome(w, http.StatusForbidden, "forbidden",
					fmt.Sprintf("patient-scoped token (%s) cannot invoke GraphDefinition/$graph for Patient/%s",
						secCtx.GetPatientId(), targetID))
				return
			}
		}

		resType = targetType
		resID = targetID
		if len(links) == 0 {
			links = postgres.DefaultGraphLinksForResource(resType)
		}
	} else {
		if inlineGraph != nil {
			startType = strings.TrimSpace(inlineGraph.Start)
			links = inlineGraph.Link
		} else if graphRef != "" {
			gd, found, err := s.resolveStoredOrBuiltinGraphDefinition(r, tenant, graphRef, resType)
			if err != nil {
				s.logger.Error("resolve GraphDefinition", "error", err, "tenant", tenant, "graph", graphRef)
				writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to resolve GraphDefinition")
				return
			}
			if !found {
				writeOperationOutcome(w, http.StatusNotFound, "not-found",
					fmt.Sprintf("GraphDefinition %q not found", graphRef))
				return
			}
			startType = strings.TrimSpace(gd.Start)
			links = gd.Link
		} else {
			links = postgres.DefaultGraphLinksForResource(resType)
		}
	}

	if startType != "" && !strings.EqualFold(startType, resType) {
		writeOperationOutcome(w, http.StatusUnprocessableEntity, "business-rule",
			fmt.Sprintf("GraphDefinition start type %q does not match target resource type %q", startType, resType))
		return
	}

	sr, err := s.store.ResourceGraph(r.Context(), postgres.ResourceGraphParams{
		TenantID:     tenant,
		ResourceType: resType,
		ResourceID:   resID,
		Links:        links,
	})
	if errors.Is(err, sql.ErrNoRows) {
		writeOperationOutcome(w, http.StatusNotFound, "not-found",
			fmt.Sprintf("%s/%s not found", resType, resID))
		return
	}
	if errors.Is(err, postgres.ErrResourceDeleted) {
		writeOperationOutcome(w, http.StatusGone, "deleted",
			fmt.Sprintf("%s/%s has been deleted", resType, resID))
		return
	}
	if err != nil {
		s.logger.Error("Resource/$graph failed", "error", err, "type", resType, "id", resID)
		writeOperationOutcome(w, http.StatusInternalServerError, "exception",
			fmt.Sprintf("failed to execute %s/%s/$graph", resType, resID))
		return
	}

	if secCtx, hasSecCtx := auth.FromContext(r.Context()); hasSecCtx {
		sr.Matches = s.filterAuthorizedIncludes(r, tenant, secCtx, sr.Matches)
	}

	entries := make([]any, 0, len(sr.Matches))
	for _, m := range sr.Matches {
		resMap, err := s.protoToFHIRMapWithMeta(m.ResType, m.ResourceProto, m.ResVersion, m.LastUpdated)
		if err != nil {
			continue
		}
		entries = append(entries, map[string]any{
			"fullUrl":  fmt.Sprintf("%s/%s", m.ResType, m.ResID),
			"resource": resMap,
		})
	}

	bundle := map[string]any{
		"resourceType": "Bundle",
		"type":         "collection",
		"total":        len(entries),
		"entry":        entries,
	}

	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		json.NewEncoder(w).Encode(bundle)
	}
}

func parseGraphLinksFromSlice(rawLinks any) []postgres.GraphLink {
	arr, ok := rawLinks.([]any)
	if !ok || len(arr) == 0 {
		return nil
	}
	links := make([]postgres.GraphLink, 0, len(arr))
	for _, rawItem := range arr {
		lm, ok := rawItem.(map[string]any)
		if !ok {
			continue
		}
		gl := postgres.GraphLink{
			Path:        stringField(lm, "path"),
			SliceName:   stringField(lm, "sliceName"),
			Max:         stringField(lm, "max"),
			Description: stringField(lm, "description"),
		}
		if minFloat, ok := lm["min"].(float64); ok {
			gl.Min = int(minFloat)
		}
		if rawTargets, ok := lm["target"].([]any); ok {
			for _, rt := range rawTargets {
				tm, ok := rt.(map[string]any)
				if !ok {
					continue
				}
				gl.Target = append(gl.Target, postgres.GraphTarget{
					Type:   stringField(tm, "type"),
					Params: stringField(tm, "params"),
					Link:   parseGraphLinksFromSlice(tm["link"]),
				})
			}
		}
		links = append(links, gl)
	}
	return links
}

func parseGraphDefinitionFromMap(m map[string]any) *parsedGraphDefinition {
	if m == nil {
		return nil
	}
	return &parsedGraphDefinition{
		ResourceType: stringField(m, "resourceType"),
		ID:           stringField(m, "id"),
		URL:          stringField(m, "url"),
		Name:         stringField(m, "name"),
		Status:       stringField(m, "status"),
		Start:        stringField(m, "start"),
		Link:         parseGraphLinksFromSlice(m["link"]),
	}
}

func (s *Server) resolveStoredOrBuiltinGraphDefinition(r *http.Request, tenant, graphRef, resType string) (*parsedGraphDefinition, bool, error) {
	if s.store != nil {
		candID := cleanID(graphRef)
		if rec, err := s.store.ReadResourceWithMeta(r.Context(), tenant, "GraphDefinition", candID); err == nil && rec != nil {
			if m, err := s.protoToFHIRMapWithMeta("GraphDefinition", rec.ResourceProto, rec.ResVersion, rec.LastUpdated); err == nil {
				if gd := parseGraphDefinitionFromMap(m); gd != nil {
					return gd, true, nil
				}
			}
		}

		if sr, err := s.store.Search(r.Context(), postgres.SearchParams{
			TenantID: tenant,
			ResType:  "GraphDefinition",
			URIs:     map[string]string{"url": graphRef},
			Count:    1,
		}); err == nil && sr != nil && len(sr.Matches) > 0 {
			m := sr.Matches[0]
			if gMap, err := s.protoToFHIRMapWithMeta("GraphDefinition", m.ResourceProto, m.ResVersion, m.LastUpdated); err == nil {
				if gd := parseGraphDefinitionFromMap(gMap); gd != nil {
					return gd, true, nil
				}
			}
		}

		if sr, err := s.store.Search(r.Context(), postgres.SearchParams{
			TenantID: tenant,
			ResType:  "GraphDefinition",
			Strings:  map[string]string{"name:exact": strings.ToLower(graphRef)},
			Count:    1,
		}); err == nil && sr != nil && len(sr.Matches) > 0 {
			m := sr.Matches[0]
			if gMap, err := s.protoToFHIRMapWithMeta("GraphDefinition", m.ResourceProto, m.ResVersion, m.LastUpdated); err == nil {
				if gd := parseGraphDefinitionFromMap(gMap); gd != nil {
					return gd, true, nil
				}
			}
		}

		if sr, err := s.store.Search(r.Context(), postgres.SearchParams{
			TenantID: tenant,
			ResType:  "GraphDefinition",
			Count:    50,
		}); err == nil && sr != nil {
			for _, m := range sr.Matches {
				gMap, cErr := s.protoToFHIRMapWithMeta("GraphDefinition", m.ResourceProto, m.ResVersion, m.LastUpdated)
				if cErr != nil {
					continue
				}
				if gd := parseGraphDefinitionFromMap(gMap); gd != nil {
					if strings.EqualFold(gd.URL, graphRef) || strings.EqualFold(gd.Name, graphRef) || strings.EqualFold(gd.ID, candID) {
						return gd, true, nil
					}
				}
			}
		}
	}

	switch strings.ToLower(strings.TrimSpace(graphRef)) {
	case "default", "http://flint-fhir.org/fhir/graphdefinition/default":
		return &parsedGraphDefinition{
			ResourceType: "GraphDefinition",
			ID:           "default",
			Start:        resType,
			Link:         postgres.DefaultGraphLinksForResource(resType),
		}, true, nil
	case "us-core-patient-care-graph", "patient-care-graph", "patient-summary",
		"http://hl7.org/fhir/us/core/graphdefinition/us-core-patient-care-graph",
		"http://flint-fhir.org/fhir/graphdefinition/patient-care-graph":
		return &parsedGraphDefinition{
			ResourceType: "GraphDefinition",
			ID:           "us-core-patient-care-graph",
			Start:        "Patient",
			Link:         postgres.DefaultGraphLinksForResource("Patient"),
		}, true, nil
	case "encounter-care-graph", "http://flint-fhir.org/fhir/graphdefinition/encounter-care-graph":
		return &parsedGraphDefinition{
			ResourceType: "GraphDefinition",
			ID:           "encounter-care-graph",
			Start:        "Encounter",
			Link:         postgres.DefaultGraphLinksForResource("Encounter"),
		}, true, nil
	case "diagnostic-report-graph", "http://flint-fhir.org/fhir/graphdefinition/diagnostic-report-graph":
		return &parsedGraphDefinition{
			ResourceType: "GraphDefinition",
			ID:           "diagnostic-report-graph",
			Start:        "DiagnosticReport",
			Link:         postgres.DefaultGraphLinksForResource("DiagnosticReport"),
		}, true, nil
	default:
		return nil, false, nil
	}
}
