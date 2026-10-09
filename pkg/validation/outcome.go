package validation

import (
	"encoding/json"
	"net/http"
)

// IssueSeverity indicates how serious the validation issue is.
type IssueSeverity string

const (
	SeverityFatal       IssueSeverity = "fatal"
	SeverityError       IssueSeverity = "error"
	SeverityWarning     IssueSeverity = "warning"
	SeverityInformation IssueSeverity = "information"
)

// IssueCode provides a categorized classification of the validation failure.
type IssueCode string

const (
	CodeStructure     IssueCode = "structure"     // Structural issue (e.g. unknown or misplaced element)
	CodeRequired      IssueCode = "required"      // Required element is missing
	CodeValue         IssueCode = "value"         // Invalid element value or format
	CodeInvalid       IssueCode = "code-invalid"  // Code is not valid in required ValueSet
	CodeInvariant     IssueCode = "invariant"     // Rule or invariant violated
	CodeNotSupported  IssueCode = "not-supported" // Resource type or feature not supported
	CodeInformational IssueCode = "informational"
)

// Issue represents an individual finding from the validation process.
type Issue struct {
	Severity    IssueSeverity `json:"severity"`
	Code        IssueCode     `json:"code"`
	Diagnostics string        `json:"diagnostics"`
	Expression  []string      `json:"expression,omitempty"` // FHIRPath elements (e.g. "Observation.status")
}

// Outcome represents the full validation result containing one or more issues.
type Outcome struct {
	Issues []Issue `json:"issues"`
}

// NewOutcome creates an empty Outcome.
func NewOutcome() *Outcome {
	return &Outcome{Issues: make([]Issue, 0)}
}

// AddIssue appends a validation issue.
func (o *Outcome) AddIssue(severity IssueSeverity, code IssueCode, diagnostics string, expressions ...string) {
	o.Issues = append(o.Issues, Issue{
		Severity:    severity,
		Code:        code,
		Diagnostics: diagnostics,
		Expression:  expressions,
	})
}

// AddError is a convenience helper for error-level issues.
func (o *Outcome) AddError(code IssueCode, diagnostics string, expressions ...string) {
	o.AddIssue(SeverityError, code, diagnostics, expressions...)
}

// AddWarning is a convenience helper for warning-level issues.
func (o *Outcome) AddWarning(code IssueCode, diagnostics string, expressions ...string) {
	o.AddIssue(SeverityWarning, code, diagnostics, expressions...)
}

// IsValid returns true if there are no 'fatal' or 'error' severity issues.
func (o *Outcome) IsValid() bool {
	for _, issue := range o.Issues {
		if issue.Severity == SeverityFatal || issue.Severity == SeverityError {
			return false
		}
	}
	return true
}

// ToOperationOutcomeMap converts the validation outcome to a standard FHIR OperationOutcome map.
func (o *Outcome) ToOperationOutcomeMap() map[string]any {
	issues := make([]map[string]any, 0, len(o.Issues))
	for _, iss := range o.Issues {
		item := map[string]any{
			"severity":    string(iss.Severity),
			"code":        string(iss.Code),
			"diagnostics": iss.Diagnostics,
		}
		if len(iss.Expression) > 0 {
			item["expression"] = iss.Expression
		}
		issues = append(issues, item)
	}

	if len(issues) == 0 {
		issues = append(issues, map[string]any{
			"severity":    string(SeverityInformation),
			"code":        string(CodeInformational),
			"diagnostics": "All validation checks passed with zero issues.",
		})
	}

	return map[string]any{
		"resourceType": "OperationOutcome",
		"issue":        issues,
	}
}

// WriteHTTP writes the OperationOutcome as an HTTP response.
func (o *Outcome) WriteHTTP(w http.ResponseWriter, defaultStatus int) {
	status := defaultStatus
	if !o.IsValid() && status == http.StatusOK {
		status = http.StatusBadRequest
	}
	w.Header().Set("Content-Type", "application/fhir+json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(o.ToOperationOutcomeMap())
}
