package store

import (
	"fmt"
	"strings"
)

var auditMetaEscaper = strings.NewReplacer(
	"%", "%25",
	"[", "%5B",
	"]", "%5D",
	" ", "%20",
)

var auditMetaUnescaper = strings.NewReplacer(
	"%20", " ",
	"%5D", "]",
	"%5B", "[",
	"%25", "%",
)

func escapeAuditMetaValue(v string) string {
	return auditMetaEscaper.Replace(v)
}

func unescapeAuditMetaValue(v string) string {
	return auditMetaUnescaper.Replace(v)
}

var auditMetaTagOrder = []string{
	"purposeOfUse",
	"agentIssuer",
	"actorSubject",
	"actorIssuer",
	"operatorCrossTenant",
	"requestId",
}

// EncodeAuditOutcomeDesc encodes PurposeOfUse and extended security provenance fields into
// trailing bracketed metadata tags on outcome_desc so Postgres, SQLite, and Doris preserve all
// attributes losslessly without requiring DDL migrations.
func EncodeAuditOutcomeDesc(rec AuditRecord) string {
	cleanRec := rec
	HydrateAuditOutcomeMetadata(&cleanRec)
	base := strings.TrimSpace(cleanRec.OutcomeDesc)

	values := map[string]string{
		"purposeOfUse": rec.PurposeOfUse,
		"agentIssuer":  rec.AgentIssuer,
		"actorSubject": rec.ActorSubject,
		"actorIssuer":  rec.ActorIssuer,
		"requestId":    rec.RequestID,
	}
	if rec.OperatorCrossTenant {
		values["operatorCrossTenant"] = "true"
	}

	var b strings.Builder
	b.WriteString(base)
	for _, key := range auditMetaTagOrder {
		val := values[key]
		if val == "" {
			continue
		}
		if key != "purposeOfUse" {
			val = escapeAuditMetaValue(val)
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		_, _ = fmt.Fprintf(&b, "[%s=%s]", key, val)
	}
	return b.String()
}

// HydrateAuditOutcomeMetadata extracts trailing bracketed metadata tags from rec.OutcomeDesc
// into the structured provenance fields on rec and leaves rec.OutcomeDesc clean.
func HydrateAuditOutcomeMetadata(rec *AuditRecord) {
	if rec == nil || rec.OutcomeDesc == "" {
		return
	}
	desc := strings.TrimSpace(rec.OutcomeDesc)
	for len(desc) > 0 && desc[len(desc)-1] == ']' {
		openIdx := strings.LastIndexByte(desc, '[')
		if openIdx == -1 {
			break
		}
		segment := desc[openIdx+1 : len(desc)-1]
		key, rawVal, ok := strings.Cut(segment, "=")
		if !ok || !applyAuditMetaTag(rec, key, rawVal) {
			break
		}
		desc = strings.TrimSpace(desc[:openIdx])
	}
	rec.OutcomeDesc = desc
}

func applyAuditMetaTag(rec *AuditRecord, key, rawVal string) bool {
	switch key {
	case "purposeOfUse":
		if rec.PurposeOfUse == "" {
			rec.PurposeOfUse = rawVal
		}
		return true
	case "agentIssuer":
		if rec.AgentIssuer == "" {
			rec.AgentIssuer = unescapeAuditMetaValue(rawVal)
		}
		return true
	case "actorSubject":
		if rec.ActorSubject == "" {
			rec.ActorSubject = unescapeAuditMetaValue(rawVal)
		}
		return true
	case "actorIssuer":
		if rec.ActorIssuer == "" {
			rec.ActorIssuer = unescapeAuditMetaValue(rawVal)
		}
		return true
	case "operatorCrossTenant":
		if strings.EqualFold(rawVal, "true") {
			rec.OperatorCrossTenant = true
		}
		return true
	case "requestId":
		if rec.RequestID == "" {
			rec.RequestID = unescapeAuditMetaValue(rawVal)
		}
		return true
	default:
		return false
	}
}
