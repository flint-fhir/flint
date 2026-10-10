package postgres_test

import (
	"fmt"
	"testing"

	"hegel.dev/go/hegel"

	"github.com/flint-fhir/flint/store/postgres"
)

// Property-based tests for FHIR search parameter parsing using Hegel.

const (
	datePrefixRegex     = `(eq|ne|lt|le|gt|ge|sa|eb|ap)?`
	isoDateRegex        = `(19[7-9][0-9]|20[0-2][0-9])-(0[1-9]|1[0-2])-(0[1-9]|1[0-9]|2[0-8])`
	quantityPrefixRegex = `(eq|ne|lt|le|gt|ge)?`
	decimalNumberRegex  = `([0-9]{1,4})(\.[0-9]{1,2})?`
	systemUriRegex      = `http://[a-z]{3,10}\.org/[a-z]{2,5}`
	unitCodeRegex       = `[a-zA-Z]{1,6}`
)

func TestProperty_DateParsingInvariance(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		prefix := hegel.Draw(ht, hegel.FromRegex(datePrefixRegex, true))
		isoDate := hegel.Draw(ht, hegel.FromRegex(isoDateRegex, true))

		raw := prefix + isoDate
		op, err := postgres.ParseDateOp(raw)
		if err != nil {
			ht.Fatalf("ParseDateOp(%q) failed unexpectedly: %v", raw, err)
		}

		expectedPrefix := prefix
		if expectedPrefix == "" {
			expectedPrefix = "eq"
		}

		if op.Prefix != expectedPrefix {
			ht.Fatalf("expected prefix %q, got %q", expectedPrefix, op.Prefix)
		}
	})
}

func TestProperty_QuantityParsingInvariance(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		prefix := hegel.Draw(ht, hegel.FromRegex(quantityPrefixRegex, true))
		numStr := hegel.Draw(ht, hegel.FromRegex(decimalNumberRegex, true))
		system := hegel.Draw(ht, hegel.FromRegex(systemUriRegex, true))
		code := hegel.Draw(ht, hegel.FromRegex(unitCodeRegex, true))

		raw := fmt.Sprintf("%s%s|%s|%s", prefix, numStr, system, code)
		op, err := postgres.ParseQuantityOp(raw)
		if err != nil {
			ht.Fatalf("ParseQuantityOp(%q) failed: %v", raw, err)
		}

		expectedPrefix := prefix
		if expectedPrefix == "" {
			expectedPrefix = "eq"
		}

		if op.Prefix != expectedPrefix {
			ht.Fatalf("expected prefix %q, got %q", expectedPrefix, op.Prefix)
		}
		if op.System != system {
			ht.Fatalf("expected system %q, got %q", system, op.System)
		}
		if op.Code != code {
			ht.Fatalf("expected code %q, got %q", code, op.Code)
		}
	})
}

func TestProperty_IncludeParsingInvariance(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		sourceType := hegel.Draw(ht, hegel.FromRegex(`(Patient|Observation|Encounter|Condition|Practitioner)`, true))
		paramName := hegel.Draw(ht, hegel.FromRegex(`[a-z]{3,10}`, true))
		targetType := hegel.Draw(ht, hegel.FromRegex(`(Patient|Practitioner|Organization)`, true))

		raw := fmt.Sprintf("%s:%s:%s", sourceType, paramName, targetType)
		inc, err := postgres.ParseInclude(raw)
		if err != nil {
			ht.Fatalf("ParseInclude(%q) failed: %v", raw, err)
		}

		if inc.SourceType != sourceType || inc.ParamName != paramName || inc.TargetType != targetType {
			ht.Fatalf("ParseInclude mismatch: got %+v, want %s:%s:%s", inc, sourceType, paramName, targetType)
		}
	})
}

func TestProperty_ETagRoundTripInvariance(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		version := hegel.Draw(ht, hegel.Integers(1, 1_000_000))
		etag := postgres.FormatETag(version)

		parsed, err := postgres.ParseETagVersion(etag)
		if err != nil {
			ht.Fatalf("ParseETagVersion(%q) failed: %v", etag, err)
		}
		if parsed != version {
			ht.Fatalf("expected version %d, got %d", version, parsed)
		}

		// Strong ETag and bare version should also parse to the same version
		strongParsed, err := postgres.ParseETagVersion(fmt.Sprintf(`"%d"`, version))
		if err != nil || strongParsed != version {
			ht.Fatalf("ParseETagVersion strong failed: %v (%d)", err, strongParsed)
		}
	})
}

func TestProperty_IfNoneExistParsingInvariance(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		tenant := hegel.Draw(ht, hegel.FromRegex(`tenant-[a-z]{3,6}`, true))
		resType := hegel.Draw(ht, hegel.FromRegex(`(Patient|Observation|Encounter|Condition|Practitioner)`, true))
		system := hegel.Draw(ht, hegel.FromRegex(`http://[a-z]{3,8}\.org/mrn`, true))
		mrn := hegel.Draw(ht, hegel.FromRegex(`MRN-[0-9]{4,8}`, true))
		family := hegel.Draw(ht, hegel.FromRegex(`[A-Za-z]{3,10}`, true))

		raw := fmt.Sprintf("%s?identifier=%s|%s&family=%s", resType, system, mrn, family)
		params, err := postgres.ParseIfNoneExist(tenant, resType, raw)
		if err != nil {
			ht.Fatalf("ParseIfNoneExist(%q) failed: %v", raw, err)
		}
		if params.TenantID != tenant || params.ResType != resType {
			ht.Fatalf("unexpected tenant/resType: %s / %s", params.TenantID, params.ResType)
		}
		if params.Tokens["identifier"] != system+"|"+mrn {
			ht.Fatalf("expected identifier token %s|%s, got %q", system, mrn, params.Tokens["identifier"])
		}
		if params.Strings["family"] != family {
			ht.Fatalf("expected family string %q, got %q", family, params.Strings["family"])
		}
	})
}

func TestProperty_ParsersNeverPanicOnArbitraryFuzz(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		arbitraryInput := hegel.Draw(ht, hegel.FromRegex(`.*`, true))

		// Must never panic or crash
		_, _ = postgres.ParseDateOp(arbitraryInput)
		_, _ = postgres.ParseQuantityOp(arbitraryInput)
		_, _ = postgres.ParseInclude(arbitraryInput)
		_, _ = postgres.ParseChainedParam(arbitraryInput, arbitraryInput)
		_, _ = postgres.ParseETagVersion(arbitraryInput)
		_, _ = postgres.ParseIfNoneExist("default", "Patient", arbitraryInput)
	})
}
