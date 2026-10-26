package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/flint-fhir/flint/pkg/auth"
	"github.com/flint-fhir/flint/store/postgres"
)

const maxStoredExportJobs = 256

type exportJob struct {
	ID         string
	TenantID   string
	PatientID  string
	CreatedAt  time.Time
	RequestURL string
	TypesOrder []string
	Files      map[string][]byte // resourceType -> NDJSON bytes
	Counts     map[string]int    // resourceType -> line count
	Combined   []byte            // optional combined NDJSON
	Status     string            // "in-progress", "completed", "failed"
	Err        string
	done       chan struct{}
}

func (s *Server) storeExportJob(job *exportJob) {
	if s.store != nil && job != nil && job.Status != "in-progress" {
		_ = s.store.SaveExportJob(context.Background(), postgres.ExportJobRecord{
			ID:         job.ID,
			TenantID:   job.TenantID,
			PatientID:  job.PatientID,
			CreatedAt:  job.CreatedAt,
			RequestURL: job.RequestURL,
			TypesOrder: job.TypesOrder,
			Files:      job.Files,
			Counts:     job.Counts,
			Combined:   nil, // Per-type files are already stored in Files; avoid 2x storage bloat in Postgres
		})
	}
	s.exportMu.Lock()
	defer s.exportMu.Unlock()
	if s.exportJobs == nil {
		s.exportJobs = make(map[string]*exportJob)
	}
	if _, exists := s.exportJobs[job.ID]; !exists && len(s.exportJobs) >= maxStoredExportJobs {
		var oldestID string
		var oldestTime time.Time
		for id, j := range s.exportJobs {
			if oldestID == "" || j.CreatedAt.Before(oldestTime) {
				oldestID = id
				oldestTime = j.CreatedAt
			}
		}
		if oldestID != "" {
			delete(s.exportJobs, oldestID)
		}
	}
	s.exportJobs[job.ID] = job
}

func (s *Server) getExportJob(tenant, jobID string) *exportJob {
	s.exportMu.RLock()
	if s.exportJobs != nil {
		if job, ok := s.exportJobs[jobID]; ok && job.TenantID == tenant {
			s.exportMu.RUnlock()
			return job
		}
	}
	s.exportMu.RUnlock()

	if s.store != nil {
		if rec, err := s.store.GetExportJob(context.Background(), tenant, jobID); err == nil && rec != nil {
			return &exportJob{
				ID:         rec.ID,
				TenantID:   rec.TenantID,
				PatientID:  rec.PatientID,
				CreatedAt:  rec.CreatedAt,
				RequestURL: rec.RequestURL,
				TypesOrder: rec.TypesOrder,
				Files:      rec.Files,
				Counts:     rec.Counts,
				Combined:   rec.Combined,
			}
		}
	}
	return nil
}

func (s *Server) deleteExportJob(tenant, jobID string) bool {
	deletedDB := false
	if s.store != nil {
		deletedDB, _ = s.store.DeleteExportJob(context.Background(), tenant, jobID)
	}
	s.exportMu.Lock()
	defer s.exportMu.Unlock()
	if s.exportJobs == nil {
		return deletedDB
	}
	job, ok := s.exportJobs[jobID]
	if !ok || job.TenantID != tenant {
		return deletedDB
	}
	delete(s.exportJobs, jobID)
	return true
}

func isValidNDJSONFormat(raw string) bool {
	v := strings.ToLower(strings.TrimSpace(raw))
	if idx := strings.Index(v, ";"); idx != -1 {
		v = strings.TrimSpace(v[:idx])
	}
	switch v {
	case "", "application/fhir+ndjson", "application/ndjson", "ndjson":
		return true
	default:
		return false
	}
}

// handleSystemExport handles GET /fhir/r4/{tenant}/$export (System-level FHIR Bulk Data Access).
func (s *Server) handleSystemExport(w http.ResponseWriter, r *http.Request) {
	s.executeBulkExport(w, r, false, "")
}

// handlePatientGroupExport handles GET /fhir/r4/{tenant}/Patient/$export (Patient-level FHIR Bulk Data Access).
func (s *Server) handlePatientGroupExport(w http.ResponseWriter, r *http.Request) {
	s.executeBulkExport(w, r, true, "")
}

// handleSinglePatientExport handles GET /fhir/r4/{tenant}/{resourceType}/{id}/$export
// Supports Patient/{id}/$export (Single-Patient Compartment Bulk Export) and
// Group/{id}/$export (ONC (g)(10) Group Cohort Bulk Export).
func (s *Server) handleSinglePatientExport(w http.ResponseWriter, r *http.Request) {
	resType := r.PathValue("resourceType")
	if resType == "Group" {
		s.executeBulkExport(w, r, true, "")
		return
	}
	if resType != "" && resType != "Patient" {
		writeOperationOutcome(w, http.StatusNotFound, "not-found",
			fmt.Sprintf("$export is not supported on %s instances", resType))
		return
	}
	patientID := strings.TrimSpace(r.PathValue("id"))
	s.executeBulkExport(w, r, true, patientID)
}

func (s *Server) executeBulkExport(w http.ResponseWriter, r *http.Request, isPatientExport bool, patientID string) {
	tenant := r.PathValue("tenant")
	q := r.URL.Query()

	if outputFormat := q.Get("_outputFormat"); !isValidNDJSONFormat(outputFormat) {
		writeOperationOutcome(w, http.StatusBadRequest, "invalid",
			fmt.Sprintf("unsupported _outputFormat %q: only application/fhir+ndjson is supported", outputFormat))
		return
	}

	var requestedTypes []string
	if typeRaw := strings.TrimSpace(q.Get("_type")); typeRaw != "" {
		seen := make(map[string]bool)
		for part := range strings.SplitSeq(typeRaw, ",") {
			trimmed := strings.TrimSpace(part)
			if trimmed == "" {
				continue
			}
			if _, ok := s.protoRegistry[trimmed]; !ok {
				writeOperationOutcome(w, http.StatusBadRequest, "invalid",
					fmt.Sprintf("unsupported resource type %q in _type parameter", trimmed))
				return
			}
			if !seen[trimmed] {
				seen[trimmed] = true
				requestedTypes = append(requestedTypes, trimmed)
			}
		}
	}

	var since *time.Time
	if sinceRaw := strings.TrimSpace(q.Get("_since")); sinceRaw != "" {
		dOp, err := postgres.ParseDateOp(sinceRaw)
		if err != nil {
			writeOperationOutcome(w, http.StatusBadRequest, "invalid",
				fmt.Sprintf("invalid _since parameter %q: %v", sinceRaw, err))
			return
		}
		t := dOp.Value
		since = &t
	}

	secCtx, hasSecCtx := auth.FromContext(r.Context())
	if hasSecCtx {
		if auth.IsPatientRestricted(secCtx) {
			allowedPat := cleanID(secCtx.GetPatientId())
			if allowedPat == "" {
				writeOperationOutcome(w, http.StatusForbidden, "forbidden",
					"patient-scoped token is missing required patient_id claim")
				return
			}
			if !isPatientExport || patientID == "" {
				writeOperationOutcome(w, http.StatusForbidden, "forbidden",
					"patient-scoped token cannot invoke system-level or multi-patient $export; use /Patient/{id}/$export")
				return
			}
			if cleanID(patientID) != allowedPat {
				writeOperationOutcome(w, http.StatusForbidden, "forbidden",
					fmt.Sprintf("patient-scoped token (%s) cannot export patient %s", secCtx.GetPatientId(), patientID))
				return
			}
			patientID = allowedPat
		}

		if len(requestedTypes) > 0 {
			for _, rt := range requestedTypes {
				if !auth.Allows(secCtx, rt, auth.ActionRead) && !auth.Allows(secCtx, rt, auth.ActionSearch) {
					writeOperationOutcome(w, http.StatusForbidden, "forbidden",
						fmt.Sprintf("insufficient scope for $export _type %s", rt))
					return
				}
			}
		} else {
			authorized := s.authorizedExportResourceTypes(secCtx)
			if len(authorized) == 0 {
				writeOperationOutcome(w, http.StatusForbidden, "forbidden",
					"insufficient scope for $export")
				return
			}
			requestedTypes = authorized
		}
	}

	if patientID != "" {
		setAuditPatientID(r.Context(), patientID)
	}

	if s.store == nil {
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "operational store is not configured")
		return
	}

	// Verify target Patient exists upfront for single-patient compartment export
	if patientID != "" {
		_, err := s.store.ReadResourceWithMeta(r.Context(), tenant, "Patient", patientID)
		if errors.Is(err, sql.ErrNoRows) {
			writeOperationOutcome(w, http.StatusNotFound, "not-found",
				fmt.Sprintf("Patient/%s not found", patientID))
			return
		}
		if errors.Is(err, postgres.ErrResourceDeleted) {
			writeOperationOutcome(w, http.StatusGone, "deleted",
				fmt.Sprintf("Patient/%s has been deleted", patientID))
			return
		}
		if err != nil {
			s.logger.Error("bulk export patient lookup failed", "error", err, "tenant", tenant, "patient", patientID)
			writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to execute bulk export")
			return
		}
	}

	exportParams := postgres.ExportParams{
		TenantID:           tenant,
		PatientID:          patientID,
		PatientCompartment: isPatientExport && patientID == "",
		ResourceTypes:      requestedTypes,
		Since:              since,
	}

	prefer := strings.ToLower(r.Header.Get("Prefer"))
	isAsync := strings.Contains(prefer, "respond-async") || strings.EqualFold(q.Get("_async"), "true")
	accept := strings.ToLower(r.Header.Get("Accept"))
	wantsManifest := (strings.Contains(accept, "application/json") || strings.Contains(accept, "application/fhir+json")) &&
		!strings.Contains(accept, "ndjson") || strings.EqualFold(q.Get("_format"), "manifest")

	// Fast path for direct NDJSON streaming (neither async nor manifest requested):
	// stream each resource line directly to the response writer in paginated DB batches
	// without buffering the entire export in memory or writing a job row to Postgres.
	if !isAsync && !wantsManifest {
		w.Header().Set("Content-Type", "application/fhir+ndjson; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		patientRestricted := hasSecCtx && auth.IsPatientRestricted(secCtx)
		allowedPat := ""
		if patientRestricted {
			allowedPat = cleanID(secCtx.GetPatientId())
		}
		_ = s.store.StreamExportResources(r.Context(), exportParams, func(res postgres.SearchResult) error {
			if hasSecCtx {
				if patientRestricted && patientID != "" && strings.EqualFold(res.ResType, "Patient") && cleanID(res.ResID) != allowedPat {
					return nil
				}
				if !s.resourceAllowedByGranularScope(secCtx, tenant, res.ResType, res.ResID, res.ResourceProto) {
					return nil
				}
			}
			line, ok := s.formatExportNDJSONLine(res)
			if ok {
				_, _ = w.Write(line)
			}
			return nil
		})
		return
	}

	jobID := uuid.NewString()
	jobPatientID := ""
	if hasSecCtx && auth.IsPatientRestricted(secCtx) {
		jobPatientID = cleanID(secCtx.GetPatientId())
	} else if patientID != "" {
		jobPatientID = cleanID(patientID)
	}

	base := requestBaseURL(r)
	pollLocation := fmt.Sprintf("%s/fhir/r4/%s/$export-poll/%s", base, tenant, jobID)
	w.Header().Set("Content-Location", pollLocation)

	if isAsync {
		job := &exportJob{
			ID:         jobID,
			TenantID:   tenant,
			PatientID:  jobPatientID,
			CreatedAt:  time.Now().UTC(),
			RequestURL: base + r.URL.RequestURI(),
			Status:     "in-progress",
			done:       make(chan struct{}),
		}
		s.storeExportJob(job)

		asyncCtx := context.WithoutCancel(r.Context())
		go s.populateExportJob(asyncCtx, job, exportParams, hasSecCtx, secCtx, patientID)

		w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"resourceType": "OperationOutcome",
			"issue": []map[string]any{
				{
					"severity":    "information",
					"code":        "informational",
					"diagnostics": fmt.Sprintf("Bulk export job %s accepted; poll %s", jobID, pollLocation),
				},
			},
		})
		return
	}

	// Synchronous manifest mode
	job := &exportJob{
		ID:         jobID,
		TenantID:   tenant,
		PatientID:  jobPatientID,
		CreatedAt:  time.Now().UTC(),
		RequestURL: base + r.URL.RequestURI(),
		Status:     "in-progress",
		done:       make(chan struct{}),
	}
	s.populateExportJob(r.Context(), job, exportParams, hasSecCtx, secCtx, patientID)
	if job.Err != "" {
		writeOperationOutcome(w, http.StatusInternalServerError, "exception", "failed to execute bulk export")
		return
	}
	s.writeExportManifest(w, r, job)
}

func (s *Server) formatExportNDJSONLine(res postgres.SearchResult) ([]byte, bool) {
	jsonBytes, err := s.protoToJSONWithMeta(res.ResType, res.ResourceProto, res.ResVersion, res.LastUpdated)
	if err != nil {
		s.logger.Error("proto to json in export", "error", err, "type", res.ResType, "id", res.ResID)
		return nil, false
	}
	var compactLine bytes.Buffer
	if err := json.Compact(&compactLine, jsonBytes); err != nil {
		compactLine.Reset()
		compactLine.Write(bytes.ReplaceAll(jsonBytes, []byte("\n"), []byte(" ")))
	}
	compactLine.WriteByte('\n')
	return compactLine.Bytes(), true
}

func (s *Server) populateExportJob(
	ctx context.Context,
	job *exportJob,
	exportParams postgres.ExportParams,
	hasSecCtx bool,
	secCtx *auth.SecurityContext,
	patientID string,
) {
	defer func() {
		if job.done != nil {
			close(job.done)
		}
	}()

	files := make(map[string]*bytes.Buffer)
	counts := make(map[string]int)
	var typesOrder []string

	patientRestricted := hasSecCtx && auth.IsPatientRestricted(secCtx)
	allowedPat := ""
	if patientRestricted {
		allowedPat = cleanID(secCtx.GetPatientId())
	}

	err := s.store.StreamExportResources(ctx, exportParams, func(res postgres.SearchResult) error {
		if hasSecCtx {
			if patientRestricted && patientID != "" && strings.EqualFold(res.ResType, "Patient") && cleanID(res.ResID) != allowedPat {
				return nil
			}
			if !s.resourceAllowedByGranularScope(secCtx, exportParams.TenantID, res.ResType, res.ResID, res.ResourceProto) {
				return nil
			}
		}
		line, ok := s.formatExportNDJSONLine(res)
		if !ok {
			return nil
		}
		buf, exists := files[res.ResType]
		if !exists {
			buf = &bytes.Buffer{}
			files[res.ResType] = buf
			typesOrder = append(typesOrder, res.ResType)
		}
		buf.Write(line)
		counts[res.ResType]++
		return nil
	})

	fileBytes := make(map[string][]byte, len(files))
	for rt, buf := range files {
		fileBytes[rt] = buf.Bytes()
	}

	s.exportMu.Lock()
	job.TypesOrder = typesOrder
	job.Files = fileBytes
	job.Counts = counts
	if err != nil {
		job.Status = "failed"
		job.Err = err.Error()
	} else {
		job.Status = "completed"
	}
	s.exportMu.Unlock()

	if err == nil {
		s.storeExportJob(job)
	}
}

func requestBaseURL(r *http.Request) string {
	if r == nil || r.Host == "" {
		return ""
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func (s *Server) authorizedExportResourceTypes(secCtx *auth.SecurityContext) []string {
	types := make([]string, 0, len(s.protoRegistry))
	for rt := range s.protoRegistry {
		if auth.Allows(secCtx, rt, auth.ActionRead) || auth.Allows(secCtx, rt, auth.ActionSearch) {
			types = append(types, rt)
		}
	}
	sort.Strings(types)
	return types
}

// handleExportPoll handles GET /fhir/r4/{tenant}/$export-poll/{jobId}.
func (s *Server) handleExportPoll(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	jobID := r.PathValue("jobId")

	job := s.getExportJob(tenant, jobID)
	if job == nil {
		writeOperationOutcome(w, http.StatusNotFound, "not-found",
			fmt.Sprintf("export job %s not found", jobID))
		return
	}

	if job.done != nil {
		if strings.EqualFold(r.URL.Query().Get("_nowait"), "true") {
			select {
			case <-job.done:
			default:
				w.Header().Set("X-Progress", "in-progress")
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(http.StatusAccepted)
				return
			}
		} else {
			select {
			case <-job.done:
			case <-time.After(2 * time.Second):
				w.Header().Set("X-Progress", "in-progress")
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(http.StatusAccepted)
				return
			}
		}
	}

	if !s.authorizeExportJobAccess(w, r, job, "") {
		return
	}

	if job.PatientID != "" {
		setAuditPatientID(r.Context(), job.PatientID)
	}

	if job.Status == "failed" {
		writeOperationOutcome(w, http.StatusInternalServerError, "exception",
			fmt.Sprintf("bulk export job %s failed: %s", jobID, job.Err))
		return
	}

	s.writeExportManifest(w, r, job)
}

// handleExportCancel handles DELETE /fhir/r4/{tenant}/$export-poll/{jobId}.
func (s *Server) handleExportCancel(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	jobID := r.PathValue("jobId")

	job := s.getExportJob(tenant, jobID)
	if job == nil {
		writeOperationOutcome(w, http.StatusNotFound, "not-found",
			fmt.Sprintf("export job %s not found", jobID))
		return
	}
	if job.done != nil {
		select {
		case <-job.done:
		case <-time.After(500 * time.Millisecond):
		}
	}

	if !s.authorizeExportJobAccess(w, r, job, "") {
		return
	}

	if job.PatientID != "" {
		setAuditPatientID(r.Context(), job.PatientID)
	}

	s.deleteExportJob(tenant, jobID)
	w.WriteHeader(http.StatusAccepted)
}

// handleExportFile handles GET /fhir/r4/{tenant}/$export-file/{jobId}/{fileName}.
func (s *Server) handleExportFile(w http.ResponseWriter, r *http.Request) {
	tenant := r.PathValue("tenant")
	jobID := r.PathValue("jobId")
	fileName := r.PathValue("fileName")

	job := s.getExportJob(tenant, jobID)
	if job == nil {
		writeOperationOutcome(w, http.StatusNotFound, "not-found",
			fmt.Sprintf("export job %s not found", jobID))
		return
	}
	if job.done != nil {
		select {
		case <-job.done:
		case <-time.After(2 * time.Second):
			w.Header().Set("X-Progress", "in-progress")
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusAccepted)
			return
		}
	}

	resType := strings.TrimSuffix(fileName, ".ndjson")
	if !s.authorizeExportJobAccess(w, r, job, resType) {
		return
	}

	if job.PatientID != "" {
		setAuditPatientID(r.Context(), job.PatientID)
	}

	ndjsonBytes, ok := job.Files[resType]
	if !ok {
		writeOperationOutcome(w, http.StatusNotFound, "not-found",
			fmt.Sprintf("export file %s not found in job %s", fileName, jobID))
		return
	}

	w.Header().Set("Content-Type", "application/fhir+ndjson; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(ndjsonBytes)
}

func (s *Server) authorizeExportJobAccess(w http.ResponseWriter, r *http.Request, job *exportJob, resType string) bool {
	secCtx, hasSecCtx := auth.FromContext(r.Context())
	if !hasSecCtx {
		return true
	}

	if auth.IsPatientRestricted(secCtx) {
		allowedPat := cleanID(secCtx.GetPatientId())
		if allowedPat == "" {
			writeOperationOutcome(w, http.StatusForbidden, "forbidden",
				"patient-scoped token is missing required patient_id claim")
			return false
		}
		if job.PatientID == "" || cleanID(job.PatientID) != allowedPat {
			writeOperationOutcome(w, http.StatusForbidden, "forbidden",
				fmt.Sprintf("patient-scoped token (%s) cannot access export job %s", secCtx.GetPatientId(), job.ID))
			return false
		}
	}

	if resType != "" {
		if !auth.Allows(secCtx, resType, auth.ActionRead) && !auth.Allows(secCtx, resType, auth.ActionSearch) {
			writeOperationOutcome(w, http.StatusForbidden, "forbidden",
				fmt.Sprintf("insufficient scope to download %s export file", resType))
			return false
		}
	} else {
		// Polling or cancelling a job requires read/search permission on at least one exported type (or Patient)
		allowed := auth.Allows(secCtx, "Patient", auth.ActionRead) || auth.Allows(secCtx, "Patient", auth.ActionSearch)
		if !allowed {
			for _, rt := range job.TypesOrder {
				if auth.Allows(secCtx, rt, auth.ActionRead) || auth.Allows(secCtx, rt, auth.ActionSearch) {
					allowed = true
					break
				}
			}
		}
		if !allowed {
			writeOperationOutcome(w, http.StatusForbidden, "forbidden",
				"insufficient scope to access export job")
			return false
		}
	}

	return true
}

func (s *Server) writeExportManifest(w http.ResponseWriter, r *http.Request, job *exportJob) {
	base := requestBaseURL(r)
	outputs := make([]map[string]any, 0, len(job.TypesOrder))
	for _, rt := range job.TypesOrder {
		outputs = append(outputs, map[string]any{
			"type":  rt,
			"url":   fmt.Sprintf("%s/fhir/r4/%s/$export-file/%s/%s.ndjson", base, job.TenantID, job.ID, rt),
			"count": job.Counts[rt],
		})
	}

	manifest := map[string]any{
		"transactionTime":     job.CreatedAt.Format(time.RFC3339),
		"request":             job.RequestURL,
		"requiresAccessToken": s.tokenValidator != nil,
		"output":              outputs,
		"error":               []any{},
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(manifest)
}
