package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"unicode"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/flint-fhir/flint/pkg/auth"
	automq "github.com/flint-fhir/flint/pkg/cdc"
	"github.com/flint-fhir/flint/store"
)

// ReferentialIntegrityMode controls write-time outbound reference target checking
// and delete-time inbound reference conflict checking per HL7 FHIR R4 CapabilityStatement.referencePolicy.
type ReferentialIntegrityMode string

const (
	// ReferentialIntegrityLenient allows dangling relative references on write and
	// allows deleting resources that are still referenced by other active resources.
	ReferentialIntegrityLenient ReferentialIntegrityMode = "lenient"

	// ReferentialIntegrityStrict ("enforced" in CapabilityStatement.rest.resource.referencePolicy)
	// rejects writes with dangling local relative references (422 Unprocessable Entity) and
	// rejects deletes of actively referenced resources (409 Conflict) unless ?_cascade=delete is specified.
	ReferentialIntegrityStrict ReferentialIntegrityMode = "strict"
)

var (
	errReferentialIntegrityTargetMissing = errors.New("referential integrity violation: target resource not found")
	errReferentialIntegrityDeleteBlocked = errors.New("referential integrity violation: resource is referenced by active resources")
)

// ParseReferentialIntegrityMode parses a CLI flag or environment variable value
// into a ReferentialIntegrityMode. Empty string defaults to ReferentialIntegrityLenient.
func ParseReferentialIntegrityMode(raw string) (ReferentialIntegrityMode, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "lenient", "off", "false", "disabled":
		return ReferentialIntegrityLenient, nil
	case "strict", "enforced", "true", "on":
		return ReferentialIntegrityStrict, nil
	default:
		return "", fmt.Errorf("invalid referential integrity mode %q (expected %q or %q)", raw, ReferentialIntegrityLenient, ReferentialIntegrityStrict)
	}
}

// SetReferentialIntegrity configures the server-wide referential integrity policy.
func (s *Server) SetReferentialIntegrity(mode ReferentialIntegrityMode) {
	if mode == ReferentialIntegrityStrict {
		s.referentialIntegrity.Store(1)
	} else {
		s.referentialIntegrity.Store(0)
	}
}

// ReferentialIntegrity returns the configured server-wide referential integrity policy.
func (s *Server) ReferentialIntegrity() ReferentialIntegrityMode {
	if s.referentialIntegrity.Load() == 1 {
		return ReferentialIntegrityStrict
	}
	return ReferentialIntegrityLenient
}

// effectiveReferentialIntegrity resolves the active ReferentialIntegrityMode for r,
// honoring an optional X-Flint-Referential-Integrity request header override.
func (s *Server) effectiveReferentialIntegrity(r *http.Request) ReferentialIntegrityMode {
	if r != nil {
		if hdr := strings.TrimSpace(r.Header.Get("X-Flint-Referential-Integrity")); hdr != "" {
			if m, err := ParseReferentialIntegrityMode(hdr); err == nil {
				return m
			}
		}
	}
	return s.ReferentialIntegrity()
}

// capabilityReferencePolicy returns the FHIR R4 CapabilityStatement.rest.resource.referencePolicy codes.
func (s *Server) capabilityReferencePolicy(r *http.Request) []string {
	if s.effectiveReferentialIntegrity(r) == ReferentialIntegrityStrict {
		return []string{"literal", "logical", "local", "enforced"}
	}
	return []string{"literal", "logical", "local"}
}

type localRefTarget struct {
	targetType string
	targetID   string
}

func (t localRefTarget) key() string {
	return t.targetType + "/" + t.targetID
}

func refSnakeToPascal(snake string) string {
	var b strings.Builder
	upperNext := true
	for _, r := range snake {
		if r == '_' {
			upperNext = true
			continue
		}
		if upperNext {
			b.WriteRune(unicode.ToUpper(r))
			upperNext = false
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func unwrapProtoStringField(msg protoreflect.Message) string {
	if !msg.IsValid() {
		return ""
	}
	valFD := msg.Descriptor().Fields().ByName("value")
	if valFD != nil && msg.Has(valFD) && valFD.Kind() == protoreflect.StringKind {
		return msg.Get(valFD).String()
	}
	return ""
}

func normalizeLocalTargetID(rawID string) (string, bool) {
	id := strings.TrimSpace(rawID)
	if id == "" || strings.HasPrefix(id, "#") || strings.Contains(id, "?") {
		return "", false
	}
	if histIdx := strings.Index(id, "/_history/"); histIdx != -1 {
		id = strings.TrimSpace(id[:histIdx])
	}
	if id == "" || strings.Contains(id, "/") {
		return "", false
	}
	return id, true
}

func (s *Server) parseLocalReferenceFromProtoRef(refMsg protoreflect.Message) (localRefTarget, bool) {
	if !refMsg.IsValid() {
		return localRefTarget{}, false
	}
	// Fragment (#contained) is a local contained reference, not a stored server resource.
	if fragFD := refMsg.Descriptor().Fields().ByName("fragment"); fragFD != nil && refMsg.Has(fragFD) && fragFD.Kind() == protoreflect.MessageKind {
		if strings.TrimSpace(unwrapProtoStringField(refMsg.Get(fragFD).Message())) != "" {
			return localRefTarget{}, false
		}
	}

	var out localRefTarget
	var found bool

	refMsg.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		name := string(fd.Name())
		if name == "uri" && fd.Kind() == protoreflect.MessageKind {
			raw := strings.TrimSpace(unwrapProtoStringField(v.Message()))
			if raw == "" || strings.HasPrefix(raw, "#") {
				return false
			}
			lower := strings.ToLower(raw)
			if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") || strings.HasPrefix(lower, "urn:") || strings.Contains(raw, "://") {
				return false
			}
			parts := strings.SplitN(raw, "/", 2)
			if len(parts) == 2 {
				candType := strings.TrimSpace(parts[0])
				if _, ok := s.protoRegistry[candType]; ok {
					if cleanID, okID := normalizeLocalTargetID(parts[1]); okID {
						out = localRefTarget{targetType: candType, targetID: cleanID}
						found = true
					}
				}
			}
			return false
		}
		if strings.HasSuffix(name, "_id") && fd.Kind() == protoreflect.MessageKind {
			rawID := unwrapProtoStringField(v.Message())
			candType := refSnakeToPascal(strings.TrimSuffix(name, "_id"))
			if _, ok := s.protoRegistry[candType]; ok {
				if cleanID, okID := normalizeLocalTargetID(rawID); okID {
					out = localRefTarget{targetType: candType, targetID: cleanID}
					found = true
					return false
				}
			}
		}
		return true
	})

	return out, found
}

func (s *Server) collectProtoLocalReferences(msg protoreflect.Message, seen map[string]bool, out *[]localRefTarget, depth int) {
	if !msg.IsValid() || depth > 8 {
		return
	}
	if string(msg.Descriptor().FullName()) == "google.fhir.r4.core.Reference" {
		if target, ok := s.parseLocalReferenceFromProtoRef(msg); ok {
			k := target.key()
			if !seen[k] {
				seen[k] = true
				*out = append(*out, target)
			}
		}
		return
	}
	msg.Range(func(fd protoreflect.FieldDescriptor, val protoreflect.Value) bool {
		if fd.Kind() != protoreflect.MessageKind {
			return true
		}
		// Contained resources live inside the parent resource and are validated by dom-3
		if depth == 0 && string(fd.Name()) == "contained" {
			return true
		}
		if fd.IsList() {
			list := val.List()
			for i := 0; i < list.Len(); i++ {
				s.collectProtoLocalReferences(list.Get(i).Message(), seen, out, depth+1)
			}
			return true
		}
		s.collectProtoLocalReferences(val.Message(), seen, out, depth+1)
		return true
	})
}

func (s *Server) extractOutboundLocalReferences(resType, resID string, msg protoreflect.Message, protoBytes []byte, idx *store.SearchIndexes) []localRefTarget {
	seen := make(map[string]bool)
	var targets []localRefTarget

	if msg == nil && len(protoBytes) > 0 {
		if md, ok := s.protoRegistry[resType]; ok {
			dynMsg := dynamicpb.NewMessage(md)
			if err := proto.Unmarshal(protoBytes, dynMsg); err == nil {
				msg = dynMsg
			}
		}
	}
	if msg != nil {
		s.collectProtoLocalReferences(msg, seen, &targets, 0)
	} else if idx != nil {
		for _, ref := range idx.References {
			if strings.Contains(ref.ResID, "#") || strings.Contains(ref.TargetID, "#") {
				continue
			}
			candType := strings.TrimSpace(ref.TargetType)
			if candType == "" || strings.Contains(candType, ":") || strings.HasPrefix(candType, "#") {
				continue
			}
			if _, ok := s.protoRegistry[candType]; !ok {
				continue
			}
			cleanID, okID := normalizeLocalTargetID(ref.TargetID)
			if !okID {
				continue
			}
			t := localRefTarget{targetType: candType, targetID: cleanID}
			if !seen[t.key()] {
				seen[t.key()] = true
				targets = append(targets, t)
			}
		}
	}

	// Filter out self-references (e.g. Patient/pat-1 referencing Patient/pat-1)
	filtered := targets[:0]
	for _, t := range targets {
		if strings.EqualFold(t.targetType, resType) && t.targetID == resID {
			continue
		}
		filtered = append(filtered, t)
	}
	return filtered
}

// verifyOutboundReferences checks that every local relative Reference in the resource
// resolves to an active resource in s.store (or in inBundleWrites while not in inBundleDeletes).
func (s *Server) verifyOutboundReferences(
	ctx context.Context,
	tenant, resType, resID string,
	msg protoreflect.Message,
	protoBytes []byte,
	idx *store.SearchIndexes,
	inBundleWrites map[string]bool,
	inBundleDeletes map[string]bool,
) error {
	if s.store == nil {
		return nil
	}
	targets := s.extractOutboundLocalReferences(resType, resID, msg, protoBytes, idx)
	if len(targets) == 0 {
		return nil
	}

	var missing []string
	for _, t := range targets {
		key := t.key()
		if inBundleDeletes != nil && inBundleDeletes[key] && (inBundleWrites == nil || !inBundleWrites[key]) {
			missing = append(missing, key)
			continue
		}
		if inBundleWrites != nil && inBundleWrites[key] {
			continue
		}
		rec, err := s.store.ReadResourceWithMeta(ctx, tenant, t.targetType, t.targetID)
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, store.ErrResourceDeleted) || (rec != nil && rec.IsDeleted) {
			missing = append(missing, key)
			continue
		}
		if err != nil {
			return fmt.Errorf("check reference target %s: %w", key, err)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: %s/%s references non-existent target(s): %s",
			errReferentialIntegrityTargetMissing, resType, resID, strings.Join(missing, ", "))
	}
	return nil
}

// parseCascadeDelete extracts and validates the FHIR _cascade query parameter
// (supporting ?_cascade=delete or ?_cascade=true).
func parseCascadeDelete(q url.Values) (bool, error) {
	if len(q) == 0 {
		return false, nil
	}
	raw, exists := q["_cascade"]
	if !exists || len(raw) == 0 {
		return false, nil
	}
	val := strings.ToLower(strings.TrimSpace(raw[0]))
	switch val {
	case "", "none", "false":
		return false, nil
	case "delete", "true":
		return true, nil
	default:
		return false, fmt.Errorf("invalid _cascade parameter %q: expected 'delete'", raw[0])
	}
}

// stripCascadeFromRawQuery parses and removes _cascade from a Bundle entry request query string.
func stripCascadeFromRawQuery(rawQuery string) (cleanedQuery string, cascade bool, err error) {
	if strings.TrimSpace(rawQuery) == "" {
		return "", false, nil
	}
	q, pErr := url.ParseQuery(rawQuery)
	if pErr != nil {
		return rawQuery, false, nil
	}
	cascade, err = parseCascadeDelete(q)
	if err != nil {
		return "", false, err
	}
	if _, has := q["_cascade"]; !has {
		return rawQuery, false, nil
	}
	delete(q, "_cascade")
	return q.Encode(), cascade, nil
}

// findInboundReferencingResources returns active resources in tenant that reference resType/resID.
// When iterate is true (for ?_cascade=delete), transitive reverse references are included wave-by-wave
// so deleting the returned slice in reverse order deletes leaf dependents before intermediate ones.
func (s *Server) findInboundReferencingResources(
	ctx context.Context,
	tenant, resType, resID string,
	iterate bool,
) ([]store.SearchResult, error) {
	if s.store == nil || resID == "" {
		return nil, nil
	}
	sr, err := s.store.Search(ctx, store.SearchParams{
		TenantID: tenant,
		ResType:  resType,
		ID:       resID,
		Count:    1,
		RevIncludes: []store.IncludeParam{
			{
				SourceType: "*",
				ParamName:  "*",
				Iterate:    iterate,
			},
		},
	})
	if err != nil {
		return nil, err
	}
	var out []store.SearchResult
	seen := make(map[string]bool)
	for _, inc := range sr.Includes {
		if strings.EqualFold(inc.ResType, resType) && inc.ResID == resID {
			continue
		}
		key := inc.ResType + "/" + inc.ResID
		if !seen[key] {
			seen[key] = true
			out = append(out, inc)
		}
	}
	return out, nil
}

// enforceInboundDeleteOrCascade checks inbound references before deleting resType/resID.
//   - If cascade is true: soft-deletes all transitively referencing resources in reverse dependency order
//     and returns the count of cascade-deleted dependents.
//   - Else if mode == ReferentialIntegrityStrict: returns errReferentialIntegrityDeleteBlocked (409 Conflict)
//     if any active resource (not in exemptSet) still references resType/resID.
func (s *Server) enforceInboundDeleteOrCascade(
	ctx context.Context,
	tenant, resType, resID string,
	mode ReferentialIntegrityMode,
	cascade bool,
	exemptSet map[string]bool,
) (int, error) {
	if s.store == nil || resID == "" || (!cascade && mode != ReferentialIntegrityStrict) {
		return 0, nil
	}

	dependents, err := s.findInboundReferencingResources(ctx, tenant, resType, resID, cascade)
	if err != nil {
		return 0, fmt.Errorf("query inbound references for %s/%s: %w", resType, resID, err)
	}

	if len(exemptSet) > 0 {
		filtered := dependents[:0]
		for _, dep := range dependents {
			if exemptSet[dep.ResType+"/"+dep.ResID] {
				continue
			}
			filtered = append(filtered, dep)
		}
		dependents = filtered
	}

	if len(dependents) == 0 {
		return 0, nil
	}

	if !cascade {
		names := make([]string, 0, len(dependents))
		for _, dep := range dependents {
			names = append(names, dep.ResType+"/"+dep.ResID)
		}
		return 0, fmt.Errorf("%w: cannot delete %s/%s because it is referenced by %s",
			errReferentialIntegrityDeleteBlocked, resType, resID, strings.Join(names, ", "))
	}

	// Enforce patient-compartment access on every dependent before cascade-deleting
	if secCtx, ok := auth.FromContext(ctx); ok && auth.IsPatientRestricted(secCtx) {
		for _, dep := range dependents {
			if !auth.Allows(secCtx, dep.ResType, auth.ActionDelete) {
				return 0, fmt.Errorf("%w: insufficient scope to cascade-delete %s/%s", errForbiddenCompartment, dep.ResType, dep.ResID)
			}
			if strings.EqualFold(dep.ResType, "Patient") && cleanID(dep.ResID) != cleanID(secCtx.GetPatientId()) {
				return 0, fmt.Errorf("%w: %s/%s", errForbiddenCompartment, dep.ResType, dep.ResID)
			}
			if !strings.EqualFold(dep.ResType, "Patient") && store.IsPatientCompartmentResourceType(dep.ResType) {
				exists, allowed, cErr := s.checkExistingResourceCompartment(ctx, tenant, dep.ResType, dep.ResID, secCtx.GetPatientId())
				if cErr != nil {
					return 0, cErr
				}
				if exists && !allowed {
					return 0, fmt.Errorf("%w: %s/%s", errForbiddenCompartment, dep.ResType, dep.ResID)
				}
			}
		}
	}

	// Delete dependents from leaves back to direct referencers
	cascaded := 0
	for i := len(dependents) - 1; i >= 0; i-- {
		dep := dependents[i]
		delRec, dErr := s.store.DeleteResourceWithMeta(ctx, tenant, dep.ResType, dep.ResID)
		if dErr != nil && !errors.Is(dErr, sql.ErrNoRows) {
			return cascaded, fmt.Errorf("cascade delete %s/%s: %w", dep.ResType, dep.ResID, dErr)
		}
		if delRec != nil && delRec.ResVersion > 0 {
			cascaded++
			s.syncSearchParameterOnDelete(dep.ResType, dep.ResID)
			s.publishCDC(ctx, automq.Message{
				TenantID: tenant,
				ResType:  dep.ResType,
				ResID:    dep.ResID,
				Op:       "D",
			})
		}
	}
	return cascaded, nil
}

func bundleErrorResponseWithCode(status int, code string, message string) map[string]any {
	return map[string]any{
		"response": map[string]any{
			"status": fmt.Sprintf("%d %s", status, http.StatusText(status)),
			"outcome": map[string]any{
				"resourceType": "OperationOutcome",
				"issue": []map[string]any{
					{"severity": "error", "code": code, "diagnostics": message},
				},
			},
		},
	}
}
