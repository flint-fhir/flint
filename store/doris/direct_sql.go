package doris

import (
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/flint-fhir/flint/store"
)

const (
	dorisResourceSelectCols = `tenant_id, res_type, res_id, res_version, is_deleted, last_updated, COALESCE(idempotency_key, ''), COALESCE(resource_proto_hex, ''), COALESCE(patient_id, ''), COALESCE(encounter_id, ''), COALESCE(primary_code_sys, ''), COALESCE(primary_code, ''), primary_date_low, primary_date_high, primary_quantity, COALESCE(primary_unit_sys, ''), COALESCE(primary_unit_code, ''), string_tokens, code_tokens, reference_tokens, uri_tokens`

	dorisNSOAuthAuthCode = "_oauth_auth_code"
	dorisNSOAuthRefresh  = "_oauth_refresh_token"
	dorisNSOAuthRevoked  = "_oauth_revoked_token"
	dorisNSReplayNonce   = "_replay_nonce"
)

func nullablePtrTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

func parseDorisStringArray(raw any) []string {
	if raw == nil {
		return nil
	}
	var str string
	switch v := raw.(type) {
	case []string:
		return slices.Clone(v)
	case []byte:
		str = strings.TrimSpace(string(v))
	case string:
		str = strings.TrimSpace(v)
	default:
		return nil
	}
	if str == "" || str == "[]" || strings.EqualFold(str, "null") {
		return nil
	}
	var parsed []string
	if err := json.Unmarshal([]byte(str), &parsed); err == nil {
		return parsed
	}
	// Fallback for single-quoted Doris array format ['a', 'b']
	if strings.HasPrefix(str, "[") && strings.HasSuffix(str, "]") {
		inner := strings.TrimSpace(str[1 : len(str)-1])
		if inner == "" {
			return nil
		}
		r := csv.NewReader(strings.NewReader(inner))
		r.TrimLeadingSpace = true
		if fields, err := r.Read(); err == nil {
			out := make([]string, 0, len(fields))
			for _, f := range fields {
				f = strings.TrimSpace(f)
				if len(f) >= 2 && f[0] == '\'' && f[len(f)-1] == '\'' {
					f = f[1 : len(f)-1]
				}
				if f != "" {
					out = append(out, f)
				}
			}
			return out
		}
	}
	return nil
}

func synthesizeIndexesFromRow(row DorisResourceRow) *store.SearchIndexes {
	idx := &store.SearchIndexes{}
	for _, tok := range row.StringTokens {
		if spName, val, ok := strings.Cut(tok, "="); ok {
			idx.Strings = append(idx.Strings, store.SpidxString{
				TenantID: row.TenantID,
				ResType:  row.ResType,
				ResID:    row.ResID,
				SpName:   spName,
				SpValue:  val,
			})
		}
	}
	for _, tok := range row.CodeTokens {
		if spName, rest, ok := strings.Cut(tok, "="); ok {
			sys, code, _ := strings.Cut(rest, "|")
			idx.Tokens = append(idx.Tokens, store.SpidxToken{
				TenantID: row.TenantID,
				ResType:  row.ResType,
				ResID:    row.ResID,
				SpName:   spName,
				SpSystem: sys,
				SpValue:  code,
			})
		}
	}
	type refPair struct{ sp, id string }
	typedRefs := make(map[refPair]bool)
	for _, tok := range row.ReferenceTokens {
		if spName, rest, ok := strings.Cut(tok, "="); ok {
			if targetType, targetID, hasSlash := strings.Cut(rest, "/"); hasSlash {
				typedRefs[refPair{spName, targetID}] = true
				idx.References = append(idx.References, store.SpidxReference{
					TenantID:   row.TenantID,
					ResType:    row.ResType,
					ResID:      row.ResID,
					SpName:     spName,
					TargetType: targetType,
					TargetID:   targetID,
				})
			}
		}
	}
	for _, tok := range row.ReferenceTokens {
		if spName, rest, ok := strings.Cut(tok, "="); ok {
			if !strings.Contains(rest, "/") && !typedRefs[refPair{spName, rest}] {
				idx.References = append(idx.References, store.SpidxReference{
					TenantID: row.TenantID,
					ResType:  row.ResType,
					ResID:    row.ResID,
					SpName:   spName,
					TargetID: rest,
				})
			}
		}
	}
	for _, tok := range row.URITokens {
		if spName, val, ok := strings.Cut(tok, "="); ok {
			idx.URIs = append(idx.URIs, store.SpidxURI{
				TenantID: row.TenantID,
				ResType:  row.ResType,
				ResID:    row.ResID,
				SpName:   spName,
				SpValue:  val,
			})
		}
	}
	if row.PrimaryDateLow != nil {
		low := row.PrimaryDateLow.UTC()
		high := low
		if row.PrimaryDateHigh != nil {
			high = row.PrimaryDateHigh.UTC()
		}
		idx.Dates = append(idx.Dates, store.SpidxDate{
			TenantID: row.TenantID,
			ResType:  row.ResType,
			ResID:    row.ResID,
			SpName:   "date",
			SpLow:    low,
			SpHigh:   high,
		})
	}
	if row.PrimaryQuantity != nil {
		idx.Quantities = append(idx.Quantities, store.SpidxQuantity{
			TenantID: row.TenantID,
			ResType:  row.ResType,
			ResID:    row.ResID,
			SpName:   "value-quantity",
			SpSystem: row.PrimaryUnitSys,
			SpCode:   row.PrimaryUnitCode,
			SpValue:  *row.PrimaryQuantity,
		})
	}
	return idx
}

func scanDorisStoredEntry(rows *sql.Rows, extractors map[string]store.IndexExtractorFunc) (storedEntry, error) {
	var (
		tenantID, resType, resID, idemKey, protoHex          string
		patID, encID, priCodeSys, priCode, priUnitSys, priUC string
		resVersion                                           int
		rawDeleted, rawLastUpd, rawDateLow, rawDateHigh      any
		rawQty                                               sql.NullFloat64
		rawStrToks, rawCodeToks, rawRefToks, rawURIToks      any
	)
	if err := rows.Scan(
		&tenantID, &resType, &resID, &resVersion, &rawDeleted, &rawLastUpd,
		&idemKey, &protoHex, &patID, &encID, &priCodeSys, &priCode,
		&rawDateLow, &rawDateHigh, &rawQty, &priUnitSys, &priUC,
		&rawStrToks, &rawCodeToks, &rawRefToks, &rawURIToks,
	); err != nil {
		return storedEntry{}, err
	}
	isDeleted := parseDorisBool(rawDeleted)
	lu := parseDorisTime(rawLastUpd)
	protoBytes, _ := hex.DecodeString(strings.TrimSpace(protoHex))

	row := DorisResourceRow{
		TenantID:         tenantID,
		ResType:          resType,
		ResID:            resID,
		ResVersion:       resVersion,
		IsDeleted:        isDeleted,
		LastUpdated:      lu,
		IdempotencyKey:   idemKey,
		ResourceProto:    protoBytes,
		ResourceProtoHex: protoHex,
		PatientID:        patID,
		EncounterID:      encID,
		PrimaryCodeSys:   priCodeSys,
		PrimaryCode:      priCode,
		PrimaryUnitSys:   priUnitSys,
		PrimaryUnitCode:  priUC,
		StringTokens:     parseDorisStringArray(rawStrToks),
		CodeTokens:       parseDorisStringArray(rawCodeToks),
		ReferenceTokens:  parseDorisStringArray(rawRefToks),
		URITokens:        parseDorisStringArray(rawURIToks),
	}
	if t := parseDorisTime(rawDateLow); !t.IsZero() {
		row.PrimaryDateLow = &t
	}
	if t := parseDorisTime(rawDateHigh); !t.IsZero() {
		row.PrimaryDateHigh = &t
	}
	if rawQty.Valid {
		v := rawQty.Float64
		row.PrimaryQuantity = &v
	}

	var idx *store.SearchIndexes
	if !isDeleted && len(protoBytes) > 0 {
		if ext := extractors[resType]; ext != nil {
			idx, _ = ext(tenantID, resID, protoBytes)
		}
	}

	hasSQLTokens := len(row.StringTokens) > 0 || len(row.CodeTokens) > 0 || len(row.ReferenceTokens) > 0 || len(row.URITokens) > 0
	if idx == nil {
		if !isDeleted {
			idx = synthesizeIndexesFromRow(row)
		}
	} else if !hasSQLTokens {
		// Fallback when rows were written without explicit SearchIndexes: hydrate tokens from proto extractor.
		extracted := ExtractRow(store.ResourceInput{
			TenantID:       tenantID,
			ResType:        resType,
			ResID:          resID,
			IdempotencyKey: idemKey,
			ResourceProto:  protoBytes,
			SearchIndexes:  idx,
		}, resVersion, lu, isDeleted)
		row = extracted
		if row.PatientID == "" {
			row.PatientID = patID
		}
		if row.EncounterID == "" {
			row.EncounterID = encID
		}
	} else if !isDeleted {
		// Keep authoritative persisted SQL token arrays while preserving multi-value Dates/Quantities
		// from proto extraction and merging SQL tokens into idx for advanced evaluator & AggregateMeta.
		syn := synthesizeIndexesFromRow(row)
		mergeSearchIndexes(idx, syn)
	}

	return storedEntry{row: row, indexes: idx}, nil
}

func mergeSearchIndexes(dst, src *store.SearchIndexes) {
	if dst == nil || src == nil {
		return
	}
	dst.Strings = append(dst.Strings, src.Strings...)
	dst.Tokens = append(dst.Tokens, src.Tokens...)
	dst.References = append(dst.References, src.References...)
	dst.URIs = append(dst.URIs, src.URIs...)
}

func (s *Store) readFullRowSQL(ctx context.Context, resTbl, tenantID, resType, resID string) (*DorisResourceRow, error) {
	query := fmt.Sprintf(
		`SELECT %s FROM %s WHERE tenant_id = ? AND res_type = ? AND res_id = ? LIMIT 1`,
		dorisResourceSelectCols, resTbl,
	)
	rows, err := s.queryDorisSQL(ctx, query, tenantID, resType, resID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, sql.ErrNoRows
	}
	entry, err := scanDorisStoredEntry(rows, store.DefaultIndexExtractors())
	if err != nil {
		return nil, err
	}
	return &entry.row, nil
}

func (s *Store) readResourceWithMetaSQL(ctx context.Context, tenantID, resType, resID string) (*store.ResourceRecord, error) {
	resTbl, err := s.tenantTableRef(ctx, tenantID, "fhir_resource")
	if err != nil {
		return nil, err
	}
	query := fmt.Sprintf(
		`SELECT res_version, is_deleted, last_updated, COALESCE(resource_proto_hex, '') FROM %s WHERE tenant_id = ? AND res_type = ? AND res_id = ? LIMIT 1`,
		resTbl,
	)
	rows, err := s.queryDorisSQL(ctx, query, tenantID, resType, resID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, sql.ErrNoRows
	}
	var (
		resVersion             int
		rawDeleted, rawLastUpd any
		protoHex               string
	)
	if err := rows.Scan(&resVersion, &rawDeleted, &rawLastUpd, &protoHex); err != nil {
		return nil, err
	}
	protoBytes, _ := hex.DecodeString(strings.TrimSpace(protoHex))
	isDeleted := parseDorisBool(rawDeleted)
	rec := &store.ResourceRecord{
		TenantID:      tenantID,
		ResType:       resType,
		ResID:         resID,
		ResVersion:    resVersion,
		LastUpdated:   parseDorisTime(rawLastUpd),
		ResourceProto: protoBytes,
		IsDeleted:     isDeleted,
	}
	if isDeleted {
		return rec, store.ErrResourceDeleted
	}
	return rec, nil
}

func (s *Store) readResourceVersionSQL(ctx context.Context, tenantID, resType, resID string, version int) (*store.ResourceRecord, error) {
	histTbl, err := s.tenantTableRef(ctx, tenantID, "fhir_resource_history")
	if err != nil {
		return nil, err
	}
	query := fmt.Sprintf(
		`SELECT is_deleted, last_updated, COALESCE(resource_proto_hex, '') FROM %s WHERE tenant_id = ? AND res_type = ? AND res_id = ? AND res_version = ? LIMIT 1`,
		histTbl,
	)
	rows, err := s.queryDorisSQL(ctx, query, tenantID, resType, resID, version)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, sql.ErrNoRows
	}
	var (
		rawDeleted, rawLastUpd any
		protoHex               string
	)
	if err := rows.Scan(&rawDeleted, &rawLastUpd, &protoHex); err != nil {
		return nil, err
	}
	protoBytes, _ := hex.DecodeString(strings.TrimSpace(protoHex))
	isDeleted := parseDorisBool(rawDeleted)
	rec := &store.ResourceRecord{
		TenantID:      tenantID,
		ResType:       resType,
		ResID:         resID,
		ResVersion:    version,
		LastUpdated:   parseDorisTime(rawLastUpd),
		ResourceProto: protoBytes,
		IsDeleted:     isDeleted,
	}
	if isDeleted {
		return rec, store.ErrResourceDeleted
	}
	return rec, nil
}

func (s *Store) listHistoryWithParamsSQL(ctx context.Context, params store.HistoryParams) ([]store.ResourceRecord, int, error) {
	limit := params.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > store.MaxSearchCount {
		limit = store.MaxSearchCount
	}
	offset := max(params.Offset, 0)

	histTbl, err := s.tenantTableRef(ctx, params.TenantID, "fhir_resource_history")
	if err != nil {
		return nil, 0, err
	}
	conds := []string{"tenant_id = ?"}
	args := []any{params.TenantID}
	if params.ResType != "" {
		conds = append(conds, "res_type = ?")
		args = append(args, params.ResType)
	}
	if params.ResID != "" {
		conds = append(conds, "res_id = ?")
		args = append(args, params.ResID)
	}
	if params.Since != nil && !params.Since.IsZero() {
		conds = append(conds, "last_updated >= ?")
		args = append(args, params.Since.UTC())
	}
	whereClause := strings.Join(conds, " AND ")

	countQuery := fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE %s`, histTbl, whereClause)
	countRows, err := s.queryDorisSQL(ctx, countQuery, args...)
	if err != nil {
		return nil, 0, err
	}
	var total int
	if countRows.Next() {
		if err := countRows.Scan(&total); err != nil {
			_ = countRows.Close()
			return nil, 0, err
		}
	}
	if err := countRows.Err(); err != nil {
		_ = countRows.Close()
		return nil, 0, err
	}
	_ = countRows.Close()

	if offset >= total || total == 0 {
		return []store.ResourceRecord{}, total, nil
	}

	orderClause := "last_updated DESC, res_version DESC"
	if params.ResType != "" && params.ResID != "" {
		orderClause = "res_version DESC"
	}
	listQuery := fmt.Sprintf(
		`SELECT res_type, res_id, res_version, is_deleted, last_updated, COALESCE(resource_proto_hex, '') FROM %s WHERE %s ORDER BY %s LIMIT %d OFFSET %d`,
		histTbl, whereClause, orderClause, limit, offset,
	)
	rows, err := s.queryDorisSQL(ctx, listQuery, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	out := make([]store.ResourceRecord, 0, min(limit, total-offset))
	for rows.Next() {
		var (
			resType, resID, protoHex string
			resVersion               int
			rawDeleted, rawLastUpd   any
		)
		if err := rows.Scan(&resType, &resID, &resVersion, &rawDeleted, &rawLastUpd, &protoHex); err != nil {
			return nil, 0, err
		}
		protoBytes, _ := hex.DecodeString(strings.TrimSpace(protoHex))
		out = append(out, store.ResourceRecord{
			TenantID:      params.TenantID,
			ResType:       resType,
			ResID:         resID,
			ResVersion:    resVersion,
			LastUpdated:   parseDorisTime(rawLastUpd),
			ResourceProto: protoBytes,
			IsDeleted:     parseDorisBool(rawDeleted),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

func (s *Store) writeUnlockedSQL(ctx context.Context, input store.ResourceInput, beforeMutate func(tenantID, resType, resID string) error) (*store.ResourceRecord, error) {
	resTbl, err := s.tenantTableRef(ctx, input.TenantID, "fhir_resource")
	if err != nil {
		return nil, err
	}
	histTbl, err := s.tenantTableRef(ctx, input.TenantID, "fhir_resource_history")
	if err != nil {
		return nil, err
	}

	stateQuery := fmt.Sprintf(`SELECT res_version, is_deleted FROM %s WHERE tenant_id = ? AND res_type = ? AND res_id = ? LIMIT 1`, resTbl)
	rows, err := s.queryDorisSQL(ctx, stateQuery, input.TenantID, input.ResType, input.ResID)
	if err != nil {
		return nil, err
	}
	var (
		exists     bool
		curVersion int
		curDeleted bool
	)
	if rows.Next() {
		var rawDeleted any
		if err := rows.Scan(&curVersion, &rawDeleted); err != nil {
			_ = rows.Close()
			return nil, err
		}
		exists = true
		curDeleted = parseDorisBool(rawDeleted)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()

	created := false
	if !exists {
		created = true
		if input.ExpectedVersion > 0 {
			return nil, fmt.Errorf("%w: resource does not exist (expected version %d)", store.ErrVersionConflict, input.ExpectedVersion)
		}
	} else {
		if curDeleted {
			created = true
		} else if input.CreateOnly {
			return nil, fmt.Errorf("%w: %s/%s already exists", store.ErrResourceAlreadyExists, input.ResType, input.ResID)
		}
		if input.ExpectedVersion > 0 && (curDeleted || curVersion != input.ExpectedVersion) {
			return nil, fmt.Errorf("%w: expected version %d, current version %d", store.ErrVersionConflict, input.ExpectedVersion, curVersion)
		}
	}

	if beforeMutate != nil {
		if err := beforeMutate(input.TenantID, input.ResType, input.ResID); err != nil {
			return nil, err
		}
	}

	newVersion := curVersion + 1
	now := time.Now().UTC()
	row := ExtractRow(input, newVersion, now, false)
	rec := store.ResourceRecord{
		TenantID:      input.TenantID,
		ResType:       input.ResType,
		ResID:         input.ResID,
		ResVersion:    newVersion,
		LastUpdated:   now,
		ResourceProto: append([]byte(nil), input.ResourceProto...),
		IsDeleted:     false,
		Created:       created,
	}

	upsertStmt, upsertArgs := buildInsertResourceSQL(resTbl, []DorisResourceRow{row})
	if err := s.execDorisSQL(ctx, upsertStmt, upsertArgs...); err != nil {
		return nil, fmt.Errorf("doris upsert resource: %w", err)
	}

	histStmt := fmt.Sprintf(`INSERT INTO %s (tenant_id, res_type, res_id, res_version, is_deleted, last_updated, resource_proto_hex) VALUES (?, ?, ?, ?, ?, ?, ?)`, histTbl)
	if err := s.execDorisSQL(ctx, histStmt,
		row.TenantID, row.ResType, row.ResID, row.ResVersion, row.IsDeleted, row.LastUpdated, row.ResourceProtoHex,
	); err != nil {
		return nil, fmt.Errorf("doris insert history: %w", err)
	}

	if coreTblName, ok := CoreResourceTableName(row.ResType); ok {
		coreTbl, err := s.tenantTableRef(ctx, row.TenantID, coreTblName)
		if err != nil {
			return nil, err
		}
		if coreStmt, coreArgs, ok := ProjectCoreResourceUpsertSQLQualified(coreTbl, row); ok {
			if err := s.execDorisSQL(ctx, coreStmt, coreArgs...); err != nil {
				return nil, fmt.Errorf("doris project core resource %s: %w", row.ResType, err)
			}
		}
	}
	return &rec, nil
}

func (s *Store) writeBatchSQL(ctx context.Context, inputs []store.ResourceInput) error {
	tenantID := inputs[0].TenantID
	if s.rowLocker != nil {
		seenKeys := make(map[string]struct{}, len(inputs))
		lockKeys := make([]string, 0, len(inputs))
		for _, in := range inputs {
			lk := "res:" + resKey(in.TenantID, in.ResType, in.ResID)
			if _, ok := seenKeys[lk]; !ok {
				seenKeys[lk] = struct{}{}
				lockKeys = append(lockKeys, lk)
			}
		}
		sort.Strings(lockKeys)
		releases := make([]func(), 0, len(lockKeys))
		defer func() {
			for i := len(releases) - 1; i >= 0; i-- {
				releases[i]()
			}
		}()
		for _, lk := range lockKeys {
			rel, err := s.acquireRowLock(ctx, lk)
			if err != nil {
				return err
			}
			releases = append(releases, rel)
		}
	}

	resTbl, err := s.tenantTableRef(ctx, tenantID, "fhir_resource")
	if err != nil {
		return err
	}
	histTbl, err := s.tenantTableRef(ctx, tenantID, "fhir_resource_history")
	if err != nil {
		return err
	}

	type typeIDKey struct {
		resType string
		resID   string
	}
	existingVers := make(map[typeIDKey]int, len(inputs))
	uniqueIDs := make([]string, 0, len(inputs))
	seenID := make(map[string]struct{}, len(inputs))
	for _, in := range inputs {
		if _, ok := seenID[in.ResID]; !ok {
			seenID[in.ResID] = struct{}{}
			uniqueIDs = append(uniqueIDs, in.ResID)
		}
	}
	const chunkSize = 200
	for start := 0; start < len(uniqueIDs); start += chunkSize {
		end := min(start+chunkSize, len(uniqueIDs))
		chunk := uniqueIDs[start:end]
		ph := make([]string, len(chunk))
		args := make([]any, 0, 1+len(chunk))
		args = append(args, tenantID)
		for i, id := range chunk {
			ph[i] = "?"
			args = append(args, id)
		}
		q := fmt.Sprintf(`SELECT res_type, res_id, res_version FROM %s WHERE tenant_id = ? AND res_id IN (%s)`, resTbl, strings.Join(ph, ", "))
		rows, err := s.queryDorisSQL(ctx, q, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var rt, rid string
			var ver int
			if err := rows.Scan(&rt, &rid, &ver); err != nil {
				_ = rows.Close()
				return err
			}
			existingVers[typeIDKey{rt, rid}] = ver
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		_ = rows.Close()
	}

	now := time.Now().UTC()
	rows := make([]DorisResourceRow, 0, len(inputs))
	for _, in := range inputs {
		key := typeIDKey{in.ResType, in.ResID}
		newVer := existingVers[key] + 1
		existingVers[key] = newVer
		rows = append(rows, ExtractRow(in, newVer, now, false))
	}
	return s.flushBatchSQL(ctx, tenantID, resTbl, histTbl, rows)
}

func (s *Store) deleteUnlockedSQL(ctx context.Context, tenantID, resType, resID string, expectedVersion int, beforeMutate func(tenantID, resType, resID string) error) (*store.ResourceRecord, error) {
	resTbl, err := s.tenantTableRef(ctx, tenantID, "fhir_resource")
	if err != nil {
		return nil, err
	}
	histTbl, err := s.tenantTableRef(ctx, tenantID, "fhir_resource_history")
	if err != nil {
		return nil, err
	}

	stateQuery := fmt.Sprintf(`SELECT res_version, is_deleted, last_updated FROM %s WHERE tenant_id = ? AND res_type = ? AND res_id = ? LIMIT 1`, resTbl)
	rows, err := s.queryDorisSQL(ctx, stateQuery, tenantID, resType, resID)
	if err != nil {
		return nil, err
	}
	var (
		exists     bool
		curVersion int
		curDeleted bool
		lastUpd    time.Time
	)
	if rows.Next() {
		var rawDeleted, rawLastUpd any
		if err := rows.Scan(&curVersion, &rawDeleted, &rawLastUpd); err != nil {
			_ = rows.Close()
			return nil, err
		}
		exists = true
		curDeleted = parseDorisBool(rawDeleted)
		lastUpd = parseDorisTime(rawLastUpd)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()

	if !exists {
		if expectedVersion > 0 {
			return nil, fmt.Errorf("%w: resource does not exist (expected version %d)", store.ErrVersionConflict, expectedVersion)
		}
		return nil, sql.ErrNoRows
	}
	if expectedVersion > 0 && (curDeleted || curVersion != expectedVersion) {
		return nil, fmt.Errorf("%w: expected version %d, current version %d", store.ErrVersionConflict, expectedVersion, curVersion)
	}
	if curDeleted {
		return &store.ResourceRecord{
			TenantID:    tenantID,
			ResType:     resType,
			ResID:       resID,
			ResVersion:  curVersion,
			LastUpdated: lastUpd,
			IsDeleted:   true,
		}, nil
	}

	if beforeMutate != nil {
		if err := beforeMutate(tenantID, resType, resID); err != nil {
			return nil, err
		}
	}

	newVersion := curVersion + 1
	now := time.Now().UTC()
	tombstoneRow := DorisResourceRow{
		TenantID:    tenantID,
		ResType:     resType,
		ResID:       resID,
		ResVersion:  newVersion,
		IsDeleted:   true,
		LastUpdated: now,
	}
	delStmt, delArgs := buildInsertResourceSQL(resTbl, []DorisResourceRow{tombstoneRow})
	if err := s.execDorisSQL(ctx, delStmt, delArgs...); err != nil {
		return nil, fmt.Errorf("doris tombstone upsert: %w", err)
	}
	histDelStmt := fmt.Sprintf(`INSERT INTO %s (tenant_id, res_type, res_id, res_version, is_deleted, last_updated, resource_proto_hex) VALUES (?, ?, ?, ?, TRUE, ?, '')`, histTbl)
	if err := s.execDorisSQL(ctx, histDelStmt, tenantID, resType, resID, newVersion, now); err != nil {
		return nil, fmt.Errorf("doris tombstone history: %w", err)
	}
	if coreTblName, ok := CoreResourceTableName(resType); ok {
		coreTbl, err := s.tenantTableRef(ctx, tenantID, coreTblName)
		if err != nil {
			return nil, err
		}
		if coreDelStmt, coreDelArgs, ok := ProjectCoreResourceDeleteSQLQualified(coreTbl, tenantID, resType, resID, newVersion, now); ok {
			if err := s.execDorisSQL(ctx, coreDelStmt, coreDelArgs...); err != nil {
				return nil, fmt.Errorf("doris tombstone core %s: %w", resType, err)
			}
		}
	}

	return &store.ResourceRecord{
		TenantID:    tenantID,
		ResType:     resType,
		ResID:       resID,
		ResVersion:  newVersion,
		LastUpdated: now,
		IsDeleted:   true,
	}, nil
}

func (s *Store) executeTransactionBundleSQL(ctx context.Context, ops []store.BundleOperation) ([]store.BundleOpResult, error) {
	type snapTarget struct {
		tenantID string
		resType  string
		resID    string
		prev     *DorisResourceRow
	}
	snaps := make(map[string]snapTarget)
	beforeMutate := func(tenantID, resType, resID string) error {
		k := resKey(tenantID, resType, resID)
		if _, seen := snaps[k]; seen {
			return nil
		}
		resTbl, err := s.tenantTableRef(ctx, tenantID, "fhir_resource")
		if err != nil {
			return err
		}
		row, err := s.readFullRowSQL(ctx, resTbl, tenantID, resType, resID)
		if errors.Is(err, sql.ErrNoRows) {
			snaps[k] = snapTarget{tenantID: tenantID, resType: resType, resID: resID, prev: nil}
			return nil
		}
		if err != nil {
			return err
		}
		cp := *row
		snaps[k] = snapTarget{tenantID: tenantID, resType: resType, resID: resID, prev: &cp}
		return nil
	}

	rollback := func() {
		for _, snap := range snaps {
			resTbl, _ := s.tenantTableRef(ctx, snap.tenantID, "fhir_resource")
			histTbl, _ := s.tenantTableRef(ctx, snap.tenantID, "fhir_resource_history")
			if snap.prev == nil {
				_ = s.execDorisSQL(ctx, fmt.Sprintf(`DELETE FROM %s WHERE tenant_id = ? AND res_type = ? AND res_id = ?`, resTbl), snap.tenantID, snap.resType, snap.resID)
				_ = s.execDorisSQL(ctx, fmt.Sprintf(`DELETE FROM %s WHERE tenant_id = ? AND res_type = ? AND res_id = ?`, histTbl), snap.tenantID, snap.resType, snap.resID)
				if tbl, ok := CoreResourceTableName(snap.resType); ok {
					coreTbl, _ := s.tenantTableRef(ctx, snap.tenantID, tbl)
					_ = s.execDorisSQL(ctx, fmt.Sprintf(`DELETE FROM %s WHERE tenant_id = ? AND res_id = ?`, coreTbl), snap.tenantID, snap.resID)
				}
				continue
			}
			prev := *snap.prev
			_ = s.execDorisSQL(ctx, fmt.Sprintf(`DELETE FROM %s WHERE tenant_id = ? AND res_type = ? AND res_id = ? AND res_version > ?`, histTbl), prev.TenantID, prev.ResType, prev.ResID, prev.ResVersion)
			_ = s.execDorisSQL(ctx, fmt.Sprintf(`DELETE FROM %s WHERE tenant_id = ? AND res_type = ? AND res_id = ?`, resTbl), prev.TenantID, prev.ResType, prev.ResID)
			upsertStmt, upsertArgs := buildInsertResourceSQL(resTbl, []DorisResourceRow{prev})
			_ = s.execDorisSQL(ctx, upsertStmt, upsertArgs...)
			if tbl, ok := CoreResourceTableName(prev.ResType); ok {
				coreTbl, _ := s.tenantTableRef(ctx, prev.TenantID, tbl)
				_ = s.execDorisSQL(ctx, fmt.Sprintf(`DELETE FROM %s WHERE tenant_id = ? AND res_id = ?`, coreTbl), prev.TenantID, prev.ResID)
				if prev.IsDeleted {
					if coreDelStmt, coreDelArgs, ok := ProjectCoreResourceDeleteSQLQualified(coreTbl, prev.TenantID, prev.ResType, prev.ResID, prev.ResVersion, prev.LastUpdated); ok {
						_ = s.execDorisSQL(ctx, coreDelStmt, coreDelArgs...)
					}
				} else if coreStmt, coreArgs, ok := ProjectCoreResourceUpsertSQLQualified(coreTbl, prev); ok {
					_ = s.execDorisSQL(ctx, coreStmt, coreArgs...)
				}
			}
		}
	}

	results := make([]store.BundleOpResult, 0, len(ops))
	for i, op := range ops {
		if op.OpType == store.BundleOpDelete || strings.EqualFold(op.Method, "DELETE") {
			targetID := op.Input.ResID
			if op.ConditionalParams != nil {
				condParams := *op.ConditionalParams
				condParams.Count = 2
				condParams.Offset = 0
				sr, err := s.searchSQL(ctx, condParams)
				if err != nil {
					rollback()
					return nil, fmt.Errorf("entry[%d] conditional delete search %s: %w", i, op.Input.ResType, err)
				}
				if sr.Total > 1 {
					rollback()
					return nil, fmt.Errorf("entry[%d] conditional delete %s: %w: matched %d existing resources", i, op.Input.ResType, store.ErrPreconditionFailed, sr.Total)
				}
				if sr.Total == 0 || len(sr.Matches) == 0 {
					if op.Input.ExpectedVersion > 0 {
						rollback()
						return nil, fmt.Errorf("entry[%d] conditional delete %s: %w: resource does not exist (expected version %d)", i, op.Input.ResType, store.ErrVersionConflict, op.Input.ExpectedVersion)
					}
					results = append(results, store.BundleOpResult{
						Status:  "204 No Content",
						ResType: op.Input.ResType,
					})
					continue
				}
				targetID = sr.Matches[0].ResID
				if op.VerifyDeleteMatch != nil {
					if err := op.VerifyDeleteMatch(targetID); err != nil {
						rollback()
						return nil, fmt.Errorf("entry[%d] conditional delete %s/%s: %w", i, op.Input.ResType, targetID, err)
					}
				}
			}
			rec, err := s.deleteUnlockedSQL(ctx, op.Input.TenantID, op.Input.ResType, targetID, op.Input.ExpectedVersion, beforeMutate)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				rollback()
				return nil, fmt.Errorf("entry[%d] delete %s/%s: %w", i, op.Input.ResType, targetID, err)
			}
			ver := 0
			var lastUpd time.Time
			if rec != nil {
				ver = rec.ResVersion
				lastUpd = rec.LastUpdated
			}
			results = append(results, store.BundleOpResult{
				Status:      "204 No Content",
				ResType:     op.Input.ResType,
				ResID:       targetID,
				ResVersion:  ver,
				LastUpdated: lastUpd,
				Record:      rec,
			})
			continue
		}

		if strings.EqualFold(op.Method, "POST") && op.IfNoneExist != nil {
			sr, err := s.searchSQL(ctx, *op.IfNoneExist)
			if err != nil {
				rollback()
				return nil, fmt.Errorf("entry[%d] conditional create %s: %w", i, op.Input.ResType, err)
			}
			if sr.Total > 1 {
				rollback()
				return nil, fmt.Errorf("entry[%d] conditional create %s: %w: If-None-Exist matched %d existing %s resources", i, op.Input.ResType, store.ErrPreconditionFailed, sr.Total, op.Input.ResType)
			}
			var (
				rec     *store.ResourceRecord
				created bool
			)
			if sr.Total == 1 && len(sr.Matches) == 1 {
				rec, err = s.readResourceWithMetaSQL(ctx, op.Input.TenantID, op.Input.ResType, sr.Matches[0].ResID)
			} else {
				rec, err = s.writeUnlockedSQL(ctx, op.Input, beforeMutate)
				created = true
			}
			if err != nil {
				rollback()
				return nil, fmt.Errorf("entry[%d] conditional create %s: %w", i, op.Input.ResType, err)
			}
			status := "200 OK"
			if created {
				status = "201 Created"
			}
			results = append(results, store.BundleOpResult{
				Status:        status,
				ResType:       rec.ResType,
				ResID:         rec.ResID,
				ResVersion:    rec.ResVersion,
				LastUpdated:   rec.LastUpdated,
				ResourceProto: rec.ResourceProto,
				Record:        rec,
				Created:       created,
			})
			continue
		}

		writeInput := op.Input
		if strings.EqualFold(op.Method, "PUT") && op.ConditionalParams != nil && op.BuildWriteInput != nil {
			condParams := *op.ConditionalParams
			condParams.Count = 2
			condParams.Offset = 0
			sr, err := s.searchSQL(ctx, condParams)
			if err != nil {
				rollback()
				return nil, fmt.Errorf("entry[%d] conditional update %s: %w", i, op.Input.ResType, err)
			}
			if sr.Total > 1 {
				rollback()
				return nil, fmt.Errorf("entry[%d] conditional update %s: %w: conditional update matched %d existing %s resources", i, op.Input.ResType, store.ErrPreconditionFailed, sr.Total, op.Input.ResType)
			}
			matchedID := ""
			isExisting := false
			if sr.Total == 1 && len(sr.Matches) == 1 {
				matchedID = sr.Matches[0].ResID
				isExisting = true
			}
			built, err := op.BuildWriteInput(matchedID, isExisting)
			if err != nil {
				rollback()
				return nil, fmt.Errorf("entry[%d] conditional update %s: %w", i, op.Input.ResType, err)
			}
			rec, err := s.writeUnlockedSQL(ctx, built, beforeMutate)
			if err != nil {
				rollback()
				return nil, fmt.Errorf("entry[%d] conditional update %s: %w", i, op.Input.ResType, err)
			}
			status := "200 OK"
			if rec.Created {
				status = "201 Created"
			}
			results = append(results, store.BundleOpResult{
				Status:        status,
				ResType:       rec.ResType,
				ResID:         rec.ResID,
				ResVersion:    rec.ResVersion,
				LastUpdated:   rec.LastUpdated,
				ResourceProto: rec.ResourceProto,
				Record:        rec,
				Created:       rec.Created,
			})
			continue
		}

		if strings.TrimSpace(writeInput.TenantID) == "" {
			rollback()
			return nil, fmt.Errorf("entry[%d] resource %s/%s: %w", i, writeInput.ResType, writeInput.ResID, store.ErrNoTenant)
		}
		rec, err := s.writeUnlockedSQL(ctx, writeInput, beforeMutate)
		if err != nil {
			rollback()
			return nil, fmt.Errorf("entry[%d] resource %s/%s: %w", i, writeInput.ResType, writeInput.ResID, err)
		}
		status := "200 OK"
		if strings.EqualFold(op.Method, "POST") || rec.Created {
			status = "201 Created"
		}
		results = append(results, store.BundleOpResult{
			Status:        status,
			ResType:       rec.ResType,
			ResID:         rec.ResID,
			ResVersion:    rec.ResVersion,
			LastUpdated:   rec.LastUpdated,
			ResourceProto: rec.ResourceProto,
			Record:        rec,
			Created:       rec.Created,
		})
	}
	return results, nil
}

func buildDirectSearchCandidateSQL(tableRef string, params store.SearchParams) (string, []any) {
	conds := []string{"tenant_id = ?", "is_deleted = FALSE"}
	args := []any{params.TenantID}

	if params.ResType != "" {
		conds = append(conds, "res_type = ?")
		args = append(args, params.ResType)
	} else if len(params.ResTypes) > 0 {
		ph := make([]string, len(params.ResTypes))
		for i, rt := range params.ResTypes {
			ph[i] = "?"
			args = append(args, rt)
		}
		conds = append(conds, "res_type IN ("+strings.Join(ph, ", ")+")")
	}

	if params.ID != "" {
		var ids []string
		for part := range strings.SplitSeq(params.ID, ",") {
			cleaned := strings.TrimSpace(part)
			if slash := strings.LastIndexByte(cleaned, '/'); slash != -1 {
				cleaned = cleaned[slash+1:]
			}
			if cleaned != "" {
				ids = append(ids, cleaned)
			}
		}
		if len(ids) == 1 {
			conds = append(conds, "res_id = ?")
			args = append(args, ids[0])
		} else if len(ids) > 1 {
			ph := make([]string, len(ids))
			for i, id := range ids {
				ph[i] = "?"
				args = append(args, id)
			}
			conds = append(conds, "res_id IN ("+strings.Join(ph, ", ")+")")
		}
	}

	for spName, val := range params.Tokens {
		if strings.Contains(spName, ":") {
			continue
		}
		var orParts []string
		var orArgs []any
		allExplicitSys := true
		for item := range strings.SplitSeq(val, ",") {
			item = strings.TrimSpace(item)
			if item == "" {
				continue
			}
			if !strings.Contains(item, "|") {
				allExplicitSys = false
				break
			}
			orParts = append(orParts, "array_contains(code_tokens, ?)")
			orArgs = append(orArgs, spName+"="+item)
		}
		if allExplicitSys && len(orParts) > 0 {
			conds = append(conds, "("+strings.Join(orParts, " OR ")+")")
			args = append(args, orArgs...)
		}
	}

	for spName, ref := range params.References {
		if strings.ContainsAny(spName, ":.") {
			continue
		}
		var orParts []string
		var orArgs []any
		for item := range strings.SplitSeq(ref, ",") {
			item = strings.TrimSpace(item)
			if item == "" {
				continue
			}
			orParts = append(orParts, "array_contains(reference_tokens, ?)")
			orArgs = append(orArgs, spName+"="+item)
			if slash := strings.LastIndexByte(item, '/'); slash != -1 {
				orParts = append(orParts, "array_contains(reference_tokens, ?)")
				orArgs = append(orArgs, spName+"="+item[slash+1:])
			}
		}
		if len(orParts) > 0 {
			conds = append(conds, "("+strings.Join(orParts, " OR ")+")")
			args = append(args, orArgs...)
		}
	}

	for spName, uriVal := range params.URIs {
		if strings.Contains(spName, ":") {
			continue
		}
		var orParts []string
		var orArgs []any
		for item := range strings.SplitSeq(uriVal, ",") {
			want := strings.TrimSpace(item)
			if want == "" {
				continue
			}
			orParts = append(orParts, "array_contains(uri_tokens, ?)")
			orArgs = append(orArgs, spName+"="+want)
		}
		if len(orParts) > 0 {
			conds = append(conds, "("+strings.Join(orParts, " OR ")+")")
			args = append(args, orArgs...)
		}
	}

	for spName, strVal := range params.Strings {
		if !strings.HasSuffix(spName, ":exact") {
			continue
		}
		baseName := strings.TrimSuffix(spName, ":exact")
		if strings.Contains(baseName, ":") {
			continue
		}
		var orParts []string
		var orArgs []any
		for _, item := range store.SplitSearchORValues(strVal) {
			needle := strings.ToLower(strings.TrimSpace(store.UnescapeFHIRSearchString(item)))
			if needle == "" {
				continue
			}
			orParts = append(orParts, "array_contains(string_tokens, ?)")
			orArgs = append(orArgs, baseName+"="+needle)
		}
		if len(orParts) > 0 {
			conds = append(conds, "("+strings.Join(orParts, " OR ")+")")
			args = append(args, orArgs...)
		}
	}

	query := fmt.Sprintf(
		"SELECT %s FROM %s WHERE %s ORDER BY last_updated DESC, res_id ASC",
		dorisResourceSelectCols, tableRef, strings.Join(conds, " AND "),
	)
	return query, args
}

func (s *Store) listTenantEntriesSQL(ctx context.Context, tenantID string) ([]storedEntry, error) {
	resTbl, err := s.tenantTableRef(ctx, tenantID, "fhir_resource")
	if err != nil {
		return nil, err
	}
	q := fmt.Sprintf(`SELECT %s FROM %s WHERE tenant_id = ? AND is_deleted = FALSE`, dorisResourceSelectCols, resTbl)
	rows, err := s.queryDorisSQL(ctx, q, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	extractors := store.DefaultIndexExtractors()
	var out []storedEntry
	for rows.Next() {
		entry, err := scanDorisStoredEntry(rows, extractors)
		if err != nil {
			return nil, err
		}
		out = append(out, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

type sqlViewCache struct {
	ctx        context.Context
	s          *Store
	byKey      map[string]storedEntry
	missing    map[string]bool
	tenantAll  []store.IndexedResourceView
	allFetched bool
	extractors map[string]store.IndexExtractorFunc
}

func newSQLViewCache(ctx context.Context, s *Store, seed []storedEntry) *sqlViewCache {
	c := &sqlViewCache{
		ctx:        ctx,
		s:          s,
		byKey:      make(map[string]storedEntry, len(seed)),
		missing:    make(map[string]bool),
		extractors: store.DefaultIndexExtractors(),
	}
	for _, entry := range seed {
		c.byKey[resKey(entry.row.TenantID, entry.row.ResType, entry.row.ResID)] = entry
	}
	return c
}

func (c *sqlViewCache) lookup(tenantID, resType, resID string) (store.IndexedResourceView, bool) {
	if resID == "" {
		return store.IndexedResourceView{}, false
	}
	if resType != "" {
		k := resKey(tenantID, resType, resID)
		if entry, ok := c.byKey[k]; ok && !entry.row.IsDeleted {
			return toIndexedView(entry), true
		}
		for _, entry := range c.byKey {
			if entry.row.TenantID == tenantID && !entry.row.IsDeleted && entry.row.ResID == resID && strings.EqualFold(entry.row.ResType, resType) {
				return toIndexedView(entry), true
			}
		}
		if c.missing[k] || c.allFetched {
			return store.IndexedResourceView{}, false
		}
	} else {
		for _, entry := range c.byKey {
			if entry.row.TenantID == tenantID && !entry.row.IsDeleted && entry.row.ResID == resID {
				return toIndexedView(entry), true
			}
		}
		if c.allFetched {
			return store.IndexedResourceView{}, false
		}
	}

	resTbl, err := c.s.tenantTableRef(c.ctx, tenantID, "fhir_resource")
	if err != nil {
		return store.IndexedResourceView{}, false
	}
	var (
		q    string
		args []any
	)
	if resType != "" {
		q = fmt.Sprintf(`SELECT %s FROM %s WHERE tenant_id = ? AND res_type = ? AND res_id = ? AND is_deleted = FALSE LIMIT 1`, dorisResourceSelectCols, resTbl)
		args = []any{tenantID, resType, resID}
	} else {
		q = fmt.Sprintf(`SELECT %s FROM %s WHERE tenant_id = ? AND res_id = ? AND is_deleted = FALSE LIMIT 1`, dorisResourceSelectCols, resTbl)
		args = []any{tenantID, resID}
	}
	rows, err := c.s.queryDorisSQL(c.ctx, q, args...)
	if err != nil {
		return store.IndexedResourceView{}, false
	}
	defer rows.Close()
	if !rows.Next() {
		if resType != "" {
			c.missing[resKey(tenantID, resType, resID)] = true
		}
		return store.IndexedResourceView{}, false
	}
	entry, err := scanDorisStoredEntry(rows, c.extractors)
	if err != nil {
		return store.IndexedResourceView{}, false
	}
	c.byKey[resKey(entry.row.TenantID, entry.row.ResType, entry.row.ResID)] = entry
	return toIndexedView(entry), true
}

func (c *sqlViewCache) listTenantViews(tenantID string) []store.IndexedResourceView {
	if c.allFetched {
		return c.tenantAll
	}
	entries, err := c.s.listTenantEntriesSQL(c.ctx, tenantID)
	if err != nil {
		return nil
	}
	out := make([]store.IndexedResourceView, 0, len(entries))
	for _, entry := range entries {
		c.byKey[resKey(entry.row.TenantID, entry.row.ResType, entry.row.ResID)] = entry
		out = append(out, toIndexedView(entry))
	}
	c.tenantAll = out
	c.allFetched = true
	return out
}

func (c *sqlViewCache) matchRefInRow(tenantID string, entry storedEntry, spName, rawRef string) bool {
	row := entry.row
	if strings.HasSuffix(spName, ":identifier") {
		if matchTokenInRow(row, entry.indexes, spName, rawRef) {
			return true
		}
		baseName := strings.TrimSuffix(spName, ":identifier")
		if entry.indexes == nil {
			return false
		}
		for _, r := range entry.indexes.References {
			if r.SpName != baseName {
				continue
			}
			view, ok := c.lookup(tenantID, r.TargetType, r.TargetID)
			if !ok {
				continue
			}
			targetEntry, exists := c.byKey[resKey(view.TenantID, view.ResType, view.ResID)]
			if exists && matchTokenInRow(targetEntry.row, targetEntry.indexes, "identifier", rawRef) {
				return true
			}
		}
		return false
	}
	for item := range strings.SplitSeq(rawRef, ",") {
		item = strings.TrimSpace(item)
		if slices.Contains(row.ReferenceTokens, spName+"="+item) {
			return true
		}
		if slash := strings.LastIndexByte(item, '/'); slash != -1 {
			if slices.Contains(row.ReferenceTokens, spName+"="+item[slash+1:]) {
				return true
			}
		}
	}
	return false
}

func (c *sqlViewCache) matchesSearchParams(entry storedEntry, params store.SearchParams) bool {
	row := entry.row
	if row.TenantID != params.TenantID || row.IsDeleted {
		return false
	}
	if params.ResType != "" && !strings.EqualFold(row.ResType, params.ResType) {
		return false
	}
	if len(params.ResTypes) > 0 {
		found := false
		for _, rt := range params.ResTypes {
			if strings.EqualFold(row.ResType, rt) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if params.ID != "" {
		matchedID := false
		for part := range strings.SplitSeq(params.ID, ",") {
			cleaned := strings.TrimSpace(part)
			if slash := strings.LastIndexByte(cleaned, '/'); slash != -1 {
				cleaned = cleaned[slash+1:]
			}
			if cleaned == row.ResID {
				matchedID = true
				break
			}
		}
		if !matchedID {
			return false
		}
	}
	for spName, val := range params.Tokens {
		if !matchTokenInRow(row, entry.indexes, spName, val) {
			return false
		}
	}
	for spName, vals := range params.TokenFilters {
		for _, v := range vals {
			if !matchTokenInRow(row, entry.indexes, spName, v) {
				return false
			}
		}
	}
	for spName, ref := range params.References {
		if !c.matchRefInRow(params.TenantID, entry, spName, ref) {
			return false
		}
	}
	for spName, refs := range params.ReferenceFilters {
		for _, r := range refs {
			if !c.matchRefInRow(params.TenantID, entry, spName, r) {
				return false
			}
		}
	}
	for spName, strVal := range params.Strings {
		if !matchStringInRow(row, spName, strVal) && !store.MatchIndexString(entry.indexes, spName, strVal) {
			return false
		}
	}
	for spName, strVals := range params.StringFilters {
		for _, sv := range strVals {
			if !matchStringInRow(row, spName, sv) && !store.MatchIndexString(entry.indexes, spName, sv) {
				return false
			}
		}
	}
	for spName, uriVal := range params.URIs {
		if !matchURIInRow(row, entry.indexes, spName, uriVal) {
			return false
		}
	}
	for spName, uriVals := range params.URIFilters {
		for _, uv := range uriVals {
			if !matchURIInRow(row, entry.indexes, spName, uv) {
				return false
			}
		}
	}
	return store.EvaluateAdvancedSearchParams(toIndexedView(entry), params, c.lookup, c.listTenantViews)
}

func (s *Store) searchSQL(ctx context.Context, params store.SearchParams) (*store.SearchResults, error) {
	resTbl, err := s.tenantTableRef(ctx, params.TenantID, "fhir_resource")
	if err != nil {
		return nil, err
	}
	querySQL, queryArgs := buildDirectSearchCandidateSQL(resTbl, params)
	rows, err := s.queryDorisSQL(ctx, querySQL, queryArgs...)
	if err != nil {
		return nil, err
	}

	extractors := store.DefaultIndexExtractors()
	var candidates []storedEntry
	for rows.Next() {
		entry, err := scanDorisStoredEntry(rows, extractors)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		candidates = append(candidates, entry)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()

	cache := newSQLViewCache(ctx, s, candidates)
	var matches []store.SearchResult
	for _, entry := range candidates {
		if !cache.matchesSearchParams(entry, params) {
			continue
		}
		matches = append(matches, store.SearchResult{
			ResType:       entry.row.ResType,
			ResID:         entry.row.ResID,
			ResVersion:    entry.row.ResVersion,
			LastUpdated:   entry.row.LastUpdated,
			ResourceProto: append([]byte(nil), entry.row.ResourceProto...),
		})
	}
	store.SortSearchResults(params.TenantID, matches, params.Sort, cache.lookup)

	total := len(matches)
	if strings.EqualFold(params.Summary, "count") {
		return &store.SearchResults{
			Matches: []store.SearchResult{},
			Total:   total,
		}, nil
	}

	count := params.Count
	if count <= 0 {
		count = 20
	}
	if count > store.MaxSearchCount {
		count = store.MaxSearchCount
	}
	offset := max(params.Offset, 0)
	if offset >= total {
		matches = []store.SearchResult{}
	} else {
		matches = matches[offset:min(offset+count, total)]
	}

	var includes []store.SearchResult
	if len(matches) > 0 && (len(params.Includes) > 0 || len(params.RevIncludes) > 0) {
		includes = store.ResolveSearchIncludes(
			params.TenantID,
			matches,
			params.Includes,
			params.RevIncludes,
			cache.lookup,
			cache.listTenantViews,
		)
	}

	return &store.SearchResults{
		Matches:  matches,
		Includes: includes,
		Total:    total,
	}, nil
}

func (s *Store) patientEverythingSQL(ctx context.Context, params store.PatientEverythingParams) (*store.SearchResults, error) {
	patRec, err := s.readResourceWithMetaSQL(ctx, params.TenantID, "Patient", params.PatientID)
	if err != nil {
		return nil, err
	}
	var matches []store.SearchResult
	if matchesEverythingFilters("Patient", patRec.LastUpdated, params.ResourceTypes, params.Since, params.Until) {
		matches = append(matches, store.SearchResult{
			ResType:       "Patient",
			ResID:         patRec.ResID,
			ResVersion:    patRec.ResVersion,
			LastUpdated:   patRec.LastUpdated,
			ResourceProto: patRec.ResourceProto,
		})
	}

	entries, err := s.listTenantEntriesSQL(ctx, params.TenantID)
	if err != nil {
		return nil, err
	}
	byKey := make(map[string]storedEntry, len(entries))
	for _, entry := range entries {
		byKey[resKey(entry.row.TenantID, entry.row.ResType, entry.row.ResID)] = entry
	}
	focalKey := resKey(params.TenantID, "Patient", params.PatientID)
	compKeys := map[string]bool{focalKey: true}
	refTargets := make(map[string]bool)
	if patEntry, ok := byKey[focalKey]; ok && patEntry.indexes != nil {
		for _, ref := range patEntry.indexes.References {
			if ref.TargetType != "" && ref.TargetID != "" {
				refTargets[resKey(params.TenantID, ref.TargetType, ref.TargetID)] = true
			}
		}
	}
	for key, entry := range byKey {
		if entry.row.ResType == "Patient" {
			continue
		}
		if entry.row.PatientID == params.PatientID {
			compKeys[key] = true
			if entry.indexes != nil {
				for _, ref := range entry.indexes.References {
					if ref.TargetType != "" && ref.TargetID != "" {
						refTargets[resKey(params.TenantID, ref.TargetType, ref.TargetID)] = true
					}
				}
			}
		}
	}
	for key, entry := range byKey {
		if compKeys[key] || entry.row.ResType == "Patient" {
			continue
		}
		if refTargets[key] {
			compKeys[key] = true
			continue
		}
		if entry.row.ResType == "Provenance" && entry.indexes != nil {
			for _, ref := range entry.indexes.References {
				if compKeys[resKey(params.TenantID, ref.TargetType, ref.TargetID)] {
					compKeys[key] = true
					break
				}
			}
		}
	}
	var includes []store.SearchResult
	for key := range compKeys {
		entry, ok := byKey[key]
		if !ok || entry.row.ResType == "Patient" {
			continue
		}
		if !matchesEverythingFilters(entry.row.ResType, entry.row.LastUpdated, params.ResourceTypes, params.Since, params.Until) {
			continue
		}
		includes = append(includes, store.SearchResult{
			ResType:       entry.row.ResType,
			ResID:         entry.row.ResID,
			ResVersion:    entry.row.ResVersion,
			LastUpdated:   entry.row.LastUpdated,
			ResourceProto: append([]byte(nil), entry.row.ResourceProto...),
		})
	}
	pagedIncludes, compTotal := paginateEverythingIncludes(includes, params.Offset, params.Count)
	return &store.SearchResults{
		Matches:  matches,
		Includes: pagedIncludes,
		Total:    len(matches) + compTotal,
	}, nil
}

func (s *Store) encounterEverythingSQL(ctx context.Context, params store.EncounterEverythingParams) (*store.SearchResults, error) {
	encRec, err := s.readResourceWithMetaSQL(ctx, params.TenantID, "Encounter", params.EncounterID)
	if err != nil {
		return nil, err
	}
	var matches []store.SearchResult
	if matchesEverythingFilters("Encounter", encRec.LastUpdated, params.ResourceTypes, params.Since, params.Until) {
		matches = append(matches, store.SearchResult{
			ResType:       "Encounter",
			ResID:         encRec.ResID,
			ResVersion:    encRec.ResVersion,
			LastUpdated:   encRec.LastUpdated,
			ResourceProto: encRec.ResourceProto,
		})
	}

	entries, err := s.listTenantEntriesSQL(ctx, params.TenantID)
	if err != nil {
		return nil, err
	}
	byKey := make(map[string]storedEntry, len(entries))
	for _, entry := range entries {
		byKey[resKey(entry.row.TenantID, entry.row.ResType, entry.row.ResID)] = entry
	}
	focalKey := resKey(params.TenantID, "Encounter", params.EncounterID)
	compKeys := map[string]bool{focalKey: true}
	refTargets := make(map[string]bool)
	if encEntry, ok := byKey[focalKey]; ok && encEntry.indexes != nil {
		for _, ref := range encEntry.indexes.References {
			if ref.TargetType != "" && ref.TargetID != "" {
				refTargets[resKey(params.TenantID, ref.TargetType, ref.TargetID)] = true
			}
		}
	}
	for key, entry := range byKey {
		if key == focalKey {
			continue
		}
		if entry.row.EncounterID == params.EncounterID {
			compKeys[key] = true
			if entry.indexes != nil {
				for _, ref := range entry.indexes.References {
					if ref.TargetType != "" && ref.TargetID != "" {
						refTargets[resKey(params.TenantID, ref.TargetType, ref.TargetID)] = true
					}
				}
			}
		}
	}
	for key := range byKey {
		if compKeys[key] {
			continue
		}
		if refTargets[key] {
			compKeys[key] = true
		}
	}
	var includes []store.SearchResult
	for key := range compKeys {
		if key == focalKey {
			continue
		}
		entry, ok := byKey[key]
		if !ok || !matchesEverythingFilters(entry.row.ResType, entry.row.LastUpdated, params.ResourceTypes, params.Since, params.Until) {
			continue
		}
		includes = append(includes, store.SearchResult{
			ResType:       entry.row.ResType,
			ResID:         entry.row.ResID,
			ResVersion:    entry.row.ResVersion,
			LastUpdated:   entry.row.LastUpdated,
			ResourceProto: append([]byte(nil), entry.row.ResourceProto...),
		})
	}
	pagedIncludes, compTotal := paginateEverythingIncludes(includes, params.Offset, params.Count)
	return &store.SearchResults{
		Matches:  matches,
		Includes: pagedIncludes,
		Total:    len(matches) + compTotal,
	}, nil
}

func (s *Store) groupEverythingSQL(ctx context.Context, params store.GroupEverythingParams) (*store.SearchResults, error) {
	var matches []store.SearchResult
	if grpRec, err := s.readResourceWithMetaSQL(ctx, params.TenantID, "Group", params.GroupID); err == nil {
		if matchesEverythingFilters("Group", grpRec.LastUpdated, params.ResourceTypes, params.Since, params.Until) {
			matches = append(matches, store.SearchResult{
				ResType:       "Group",
				ResID:         grpRec.ResID,
				ResVersion:    grpRec.ResVersion,
				LastUpdated:   grpRec.LastUpdated,
				ResourceProto: grpRec.ResourceProto,
			})
		}
	} else if !params.AllPatients {
		return nil, err
	}

	patientSet := make(map[string]bool, len(params.PatientIDs))
	for _, pid := range params.PatientIDs {
		patientSet[pid] = true
	}
	entries, err := s.listTenantEntriesSQL(ctx, params.TenantID)
	if err != nil {
		return nil, err
	}
	var includes []store.SearchResult
	for _, entry := range entries {
		if entry.row.ResType == "Group" {
			continue
		}
		inCohort := params.AllPatients || patientSet[entry.row.PatientID] || (entry.row.ResType == "Patient" && patientSet[entry.row.ResID])
		if !inCohort || !matchesEverythingFilters(entry.row.ResType, entry.row.LastUpdated, params.ResourceTypes, params.Since, params.Until) {
			continue
		}
		includes = append(includes, store.SearchResult{
			ResType:       entry.row.ResType,
			ResID:         entry.row.ResID,
			ResVersion:    entry.row.ResVersion,
			LastUpdated:   entry.row.LastUpdated,
			ResourceProto: append([]byte(nil), entry.row.ResourceProto...),
		})
	}
	pagedIncludes, compTotal := paginateEverythingIncludes(includes, params.Offset, params.Count)
	return &store.SearchResults{
		Matches:  matches,
		Includes: pagedIncludes,
		Total:    len(matches) + compTotal,
	}, nil
}

func (s *Store) belongsToPatientCompartmentSQL(ctx context.Context, tenantID, resType, resID, patientID string) (bool, error) {
	resTbl, err := s.tenantTableRef(ctx, tenantID, "fhir_resource")
	if err != nil {
		return false, err
	}
	q := fmt.Sprintf(`SELECT COALESCE(patient_id, '') FROM %s WHERE tenant_id = ? AND res_type = ? AND res_id = ? AND is_deleted = FALSE LIMIT 1`, resTbl)
	rows, err := s.queryDorisSQL(ctx, q, tenantID, resType, resID)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	if !rows.Next() {
		return false, rows.Err()
	}
	var pid string
	if err := rows.Scan(&pid); err != nil {
		return false, err
	}
	return pid == patientID, nil
}

func (s *Store) searchPatientMatchCandidatesSQL(ctx context.Context, params store.PatientMatchCandidateParams) ([]store.SearchResult, error) {
	resTbl, err := s.tenantTableRef(ctx, params.TenantID, "fhir_resource")
	if err != nil {
		return nil, err
	}
	conds := []string{"tenant_id = ?", "is_deleted = FALSE", "res_type = 'Patient'"}
	args := []any{params.TenantID}
	if params.RestrictedPatientID != "" {
		conds = append(conds, "res_id = ?")
		args = append(args, params.RestrictedPatientID)
	}
	q := fmt.Sprintf(`SELECT res_id, res_version, last_updated, COALESCE(resource_proto_hex, '') FROM %s WHERE %s`, resTbl, strings.Join(conds, " AND "))
	rows, err := s.queryDorisSQL(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []store.SearchResult
	for rows.Next() {
		var (
			resID, protoHex string
			resVersion      int
			rawLastUpd      any
		)
		if err := rows.Scan(&resID, &resVersion, &rawLastUpd, &protoHex); err != nil {
			return nil, err
		}
		protoBytes, _ := hex.DecodeString(strings.TrimSpace(protoHex))
		out = append(out, store.SearchResult{
			ResType:       "Patient",
			ResID:         resID,
			ResVersion:    resVersion,
			LastUpdated:   parseDorisTime(rawLastUpd),
			ResourceProto: protoBytes,
		})
	}
	return out, rows.Err()
}

func (s *Store) resourceGraphSQL(ctx context.Context, params store.ResourceGraphParams) (*store.SearchResults, error) {
	focal, err := s.readResourceWithMetaSQL(ctx, params.TenantID, params.ResourceType, params.ResourceID)
	if err != nil {
		return nil, err
	}
	matches := []store.SearchResult{{
		ResType:       focal.ResType,
		ResID:         focal.ResID,
		ResVersion:    focal.ResVersion,
		LastUpdated:   focal.LastUpdated,
		ResourceProto: focal.ResourceProto,
	}}

	entries, err := s.listTenantEntriesSQL(ctx, params.TenantID)
	if err != nil {
		return nil, err
	}
	byKey := make(map[string]storedEntry, len(entries))
	for _, entry := range entries {
		byKey[resKey(entry.row.TenantID, entry.row.ResType, entry.row.ResID)] = entry
	}
	focalKey := resKey(params.TenantID, params.ResourceType, params.ResourceID)
	seen := map[string]bool{focalKey: true}
	appendTarget := func(targetType, targetID string) {
		if targetID == "" {
			return
		}
		if targetType != "" {
			k := resKey(params.TenantID, targetType, targetID)
			if seen[k] {
				return
			}
			if entry, ok := byKey[k]; ok {
				seen[k] = true
				matches = append(matches, store.SearchResult{
					ResType:       entry.row.ResType,
					ResID:         entry.row.ResID,
					ResVersion:    entry.row.ResVersion,
					LastUpdated:   entry.row.LastUpdated,
					ResourceProto: append([]byte(nil), entry.row.ResourceProto...),
				})
			}
			return
		}
		for k, entry := range byKey {
			if entry.row.ResID == targetID && !seen[k] {
				seen[k] = true
				matches = append(matches, store.SearchResult{
					ResType:       entry.row.ResType,
					ResID:         entry.row.ResID,
					ResVersion:    entry.row.ResVersion,
					LastUpdated:   entry.row.LastUpdated,
					ResourceProto: append([]byte(nil), entry.row.ResourceProto...),
				})
			}
		}
	}

	if focalEntry, ok := byKey[focalKey]; ok && focalEntry.indexes != nil {
		for _, ref := range focalEntry.indexes.References {
			appendTarget(ref.TargetType, ref.TargetID)
		}
	}
	for k, entry := range byKey {
		if seen[k] {
			continue
		}
		linked := (strings.EqualFold(params.ResourceType, "Patient") && entry.row.PatientID == params.ResourceID) ||
			(strings.EqualFold(params.ResourceType, "Encounter") && entry.row.EncounterID == params.ResourceID)
		if !linked && entry.indexes != nil {
			for _, ref := range entry.indexes.References {
				if ref.TargetID == params.ResourceID && (ref.TargetType == "" || strings.EqualFold(ref.TargetType, params.ResourceType)) {
					linked = true
					break
				}
			}
		}
		if linked {
			seen[k] = true
			matches = append(matches, store.SearchResult{
				ResType:       entry.row.ResType,
				ResID:         entry.row.ResID,
				ResVersion:    entry.row.ResVersion,
				LastUpdated:   entry.row.LastUpdated,
				ResourceProto: append([]byte(nil), entry.row.ResourceProto...),
			})
		}
	}
	return &store.SearchResults{
		Matches: matches,
		Total:   len(matches),
	}, nil
}

func (s *Store) aggregateMetaSQL(ctx context.Context, tenantID, resType string) (*store.AggregatedMeta, error) {
	resTbl, err := s.tenantTableRef(ctx, tenantID, "fhir_resource")
	if err != nil {
		return nil, err
	}
	conds := []string{"tenant_id = ?", "is_deleted = FALSE"}
	args := []any{tenantID}
	if resType != "" {
		conds = append(conds, "res_type = ?")
		args = append(args, resType)
	}
	q := fmt.Sprintf(`SELECT %s FROM %s WHERE %s`, dorisResourceSelectCols, resTbl, strings.Join(conds, " AND "))
	rows, err := s.queryDorisSQL(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := &store.AggregatedMeta{}
	profSeen := make(map[string]bool)
	tagSeen := make(map[string]bool)
	secSeen := make(map[string]bool)
	extractors := store.DefaultIndexExtractors()

	for rows.Next() {
		entry, err := scanDorisStoredEntry(rows, extractors)
		if err != nil {
			return nil, err
		}
		if entry.indexes == nil {
			continue
		}
		for _, u := range entry.indexes.URIs {
			if u.SpName == "_profile" && u.SpValue != "" && !profSeen[u.SpValue] {
				profSeen[u.SpValue] = true
				out.Profiles = append(out.Profiles, u.SpValue)
			}
		}
		for _, t := range entry.indexes.Tokens {
			key := t.SpSystem + "|" + t.SpValue
			if t.SpName == "_tag" && !tagSeen[key] {
				tagSeen[key] = true
				out.Tags = append(out.Tags, store.MetaCodingPair{System: t.SpSystem, Code: t.SpValue})
			} else if t.SpName == "_security" && !secSeen[key] {
				secSeen[key] = true
				out.Security = append(out.Security, store.MetaCodingPair{System: t.SpSystem, Code: t.SpValue})
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Strings(out.Profiles)
	return out, nil
}

func (s *Store) streamAnalyticalExportSQL(ctx context.Context, params store.ExportParams, emit func(store.SearchResult) error) error {
	resTbl, err := s.tenantTableRef(ctx, params.TenantID, "fhir_resource")
	if err != nil {
		return err
	}
	conds := []string{"tenant_id = ?", "is_deleted = FALSE"}
	args := []any{params.TenantID}

	if len(params.ResourceTypes) > 0 {
		ph := make([]string, len(params.ResourceTypes))
		for i, rt := range params.ResourceTypes {
			ph[i] = "?"
			args = append(args, rt)
		}
		conds = append(conds, "res_type IN ("+strings.Join(ph, ", ")+")")
	}
	var patIDs []string
	for _, pid := range params.PatientIDs {
		cleaned := strings.TrimSpace(pid)
		if slash := strings.LastIndexByte(cleaned, '/'); slash != -1 {
			cleaned = cleaned[slash+1:]
		}
		if cleaned != "" {
			patIDs = append(patIDs, cleaned)
		}
	}
	if params.PatientID != "" {
		patIDs = append(patIDs, params.PatientID)
	}
	if len(patIDs) > 0 {
		ph := make([]string, len(patIDs))
		for i, pid := range patIDs {
			ph[i] = "?"
			args = append(args, pid)
		}
		conds = append(conds, "patient_id IN ("+strings.Join(ph, ", ")+")")
	}
	if params.Since != nil {
		conds = append(conds, "last_updated >= ?")
		args = append(args, params.Since.UTC())
	}
	if params.Until != nil {
		conds = append(conds, "last_updated <= ?")
		args = append(args, params.Until.UTC())
	}

	q := fmt.Sprintf(
		`SELECT res_type, res_id, res_version, last_updated, COALESCE(resource_proto_hex, '') FROM %s WHERE %s ORDER BY res_type ASC, res_id ASC`,
		resTbl, strings.Join(conds, " AND "),
	)
	rows, err := s.queryDorisSQL(ctx, q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			resType, resID, protoHex string
			resVersion               int
			rawLastUpd               any
		)
		if err := rows.Scan(&resType, &resID, &resVersion, &rawLastUpd, &protoHex); err != nil {
			return err
		}
		protoBytes, _ := hex.DecodeString(strings.TrimSpace(protoHex))
		if err := emit(store.SearchResult{
			ResType:       resType,
			ResID:         resID,
			ResVersion:    resVersion,
			LastUpdated:   parseDorisTime(rawLastUpd),
			ResourceProto: protoBytes,
		}); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (s *Store) evaluateObservationStatsSQL(ctx context.Context, params store.ObservationStatsParams) (*store.ObservationStatsResult, error) {
	obsTbl, err := s.tenantTableRef(ctx, params.TenantID, "fhir_observation")
	if err != nil {
		return nil, err
	}
	conds := []string{"tenant_id = ?", "is_deleted = FALSE", "value_quantity IS NOT NULL"}
	args := []any{params.TenantID}
	if params.PatientID != "" {
		conds = append(conds, "patient_id = ?")
		args = append(args, params.PatientID)
	}
	if params.Code != "" {
		conds = append(conds, "code = ?")
		args = append(args, params.Code)
	}
	if params.System != "" {
		conds = append(conds, "code_system = ?")
		args = append(args, params.System)
	}
	if params.Start != nil {
		conds = append(conds, "(effective_start IS NULL OR effective_start >= ?)")
		args = append(args, params.Start.UTC())
	}
	if params.End != nil {
		conds = append(conds, "(effective_end IS NULL OR effective_end <= ?)")
		args = append(args, params.End.UTC())
	}

	statsSQL := fmt.Sprintf(
		`SELECT COALESCE(code_system, ''), COALESCE(code, ''), COALESCE(value_unit_code, ''), value_quantity FROM %s WHERE %s`,
		obsTbl, strings.Join(conds, " AND "),
	)
	rows, err := s.queryDorisSQL(ctx, statsSQL, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type bucket struct {
		sys    string
		code   string
		unit   string
		values []float64
	}
	groups := make(map[string]*bucket)
	for rows.Next() {
		var sys, code, unit string
		var val float64
		if err := rows.Scan(&sys, &code, &unit, &val); err != nil {
			return nil, err
		}
		k := sys + "|" + code + "|" + unit
		b, ok := groups[k]
		if !ok {
			b = &bucket{sys: sys, code: code, unit: unit}
			groups[k] = b
		}
		b.values = append(b.values, val)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var series []store.ObservationStatsMetrics
	for _, b := range groups {
		sort.Float64s(b.values)
		n := len(b.values)
		sum := 0.0
		for _, v := range b.values {
			sum += v
		}
		avg := sum / float64(n)
		var sqDiff float64
		for _, v := range b.values {
			d := v - avg
			sqDiff += d * d
		}
		variance := 0.0
		if n > 1 {
			variance = sqDiff / float64(n-1)
		}
		median := b.values[n/2]
		if n%2 == 0 {
			median = (b.values[n/2-1] + b.values[n/2]) / 2.0
		}
		series = append(series, store.ObservationStatsMetrics{
			Code:       b.code,
			System:     b.sys,
			Unit:       b.unit,
			Count:      n,
			TotalCount: n,
			Average:    avg,
			Min:        b.values[0],
			Max:        b.values[n-1],
			Sum:        sum,
			Median:     median,
			StdDev:     math.Sqrt(variance),
			Variance:   variance,
		})
	}
	sort.Slice(series, func(i, j int) bool {
		return series[i].Code < series[j].Code
	})
	return &store.ObservationStatsResult{Series: series}, nil
}

func (s *Store) evaluateCohortCountsSQL(ctx context.Context, tenantID string, resourceTypes []string) (map[string]int, error) {
	resTbl, err := s.tenantTableRef(ctx, tenantID, "fhir_resource")
	if err != nil {
		return nil, err
	}
	conds := []string{"tenant_id = ?", "is_deleted = FALSE"}
	args := []any{tenantID}
	if len(resourceTypes) > 0 {
		ph := make([]string, len(resourceTypes))
		for i, rt := range resourceTypes {
			ph[i] = "?"
			args = append(args, rt)
		}
		conds = append(conds, "res_type IN ("+strings.Join(ph, ", ")+")")
	}
	q := fmt.Sprintf(`SELECT res_type, COUNT(*) FROM %s WHERE %s GROUP BY res_type`, resTbl, strings.Join(conds, " AND "))
	rows, err := s.queryDorisSQL(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	counts := make(map[string]int)
	for rows.Next() {
		var rt string
		var cnt int
		if err := rows.Scan(&rt, &cnt); err != nil {
			return nil, err
		}
		counts[rt] = cnt
	}
	return counts, rows.Err()
}

func (s *Store) purgePatientCompartmentSQL(ctx context.Context, tenantID, patientID string, hardExpunge bool) ([]store.ResourceRecord, error) {
	s.txMu.RLock()
	defer s.txMu.RUnlock()
	if _, err := s.readResourceWithMetaSQL(ctx, tenantID, "Patient", patientID); err != nil {
		if !errors.Is(err, store.ErrResourceDeleted) || !hardExpunge {
			return nil, err
		}
	}
	resTbl, err := s.tenantTableRef(ctx, tenantID, "fhir_resource")
	if err != nil {
		return nil, err
	}
	histTbl, err := s.tenantTableRef(ctx, tenantID, "fhir_resource_history")
	if err != nil {
		return nil, err
	}
	q := fmt.Sprintf(`SELECT res_type, res_id, res_version, is_deleted FROM %s WHERE tenant_id = ? AND patient_id = ?`, resTbl)
	rows, err := s.queryDorisSQL(ctx, q, tenantID, patientID)
	if err != nil {
		return nil, err
	}
	type purgeItem struct {
		resType   string
		resID     string
		version   int
		isDeleted bool
	}
	var items []purgeItem
	for rows.Next() {
		var rt, rid string
		var ver int
		var rawDel any
		if err := rows.Scan(&rt, &rid, &ver, &rawDel); err != nil {
			_ = rows.Close()
			return nil, err
		}
		items = append(items, purgeItem{rt, rid, ver, parseDorisBool(rawDel)})
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()

	now := time.Now().UTC()
	var purged []store.ResourceRecord
	for _, it := range items {
		if hardExpunge {
			if err := s.execDorisSQL(ctx, fmt.Sprintf(`DELETE FROM %s WHERE tenant_id = ? AND res_type = ? AND res_id = ?`, resTbl), tenantID, it.resType, it.resID); err != nil {
				return nil, err
			}
			if err := s.execDorisSQL(ctx, fmt.Sprintf(`DELETE FROM %s WHERE tenant_id = ? AND res_type = ? AND res_id = ?`, histTbl), tenantID, it.resType, it.resID); err != nil {
				return nil, err
			}
			if tbl, ok := CoreResourceTableName(it.resType); ok {
				coreTbl, _ := s.tenantTableRef(ctx, tenantID, tbl)
				_ = s.execDorisSQL(ctx, fmt.Sprintf(`DELETE FROM %s WHERE tenant_id = ? AND res_id = ?`, coreTbl), tenantID, it.resID)
			}
			purged = append(purged, store.ResourceRecord{
				TenantID:    tenantID,
				ResType:     it.resType,
				ResID:       it.resID,
				ResVersion:  it.version,
				LastUpdated: now,
				IsDeleted:   true,
			})
		} else if !it.isDeleted {
			newVer := it.version + 1
			tombRow := DorisResourceRow{
				TenantID:    tenantID,
				ResType:     it.resType,
				ResID:       it.resID,
				ResVersion:  newVer,
				IsDeleted:   true,
				LastUpdated: now,
			}
			tStmt, tArgs := buildInsertResourceSQL(resTbl, []DorisResourceRow{tombRow})
			if err := s.execDorisSQL(ctx, tStmt, tArgs...); err != nil {
				return nil, err
			}
			if err := s.execDorisSQL(ctx, fmt.Sprintf(`INSERT INTO %s (tenant_id, res_type, res_id, res_version, is_deleted, last_updated, resource_proto_hex) VALUES (?, ?, ?, ?, TRUE, ?, '')`, histTbl),
				tenantID, it.resType, it.resID, newVer, now); err != nil {
				return nil, err
			}
			if tbl, ok := CoreResourceTableName(it.resType); ok {
				coreTbl, _ := s.tenantTableRef(ctx, tenantID, tbl)
				if cStmt, cArgs, ok := ProjectCoreResourceDeleteSQLQualified(coreTbl, tenantID, it.resType, it.resID, newVer, now); ok {
					_ = s.execDorisSQL(ctx, cStmt, cArgs...)
				}
			}
			purged = append(purged, store.ResourceRecord{
				TenantID:    tenantID,
				ResType:     it.resType,
				ResID:       it.resID,
				ResVersion:  newVer,
				LastUpdated: now,
				IsDeleted:   true,
			})
		}
	}
	return purged, nil
}

func (s *Store) expungeResourcesSQL(ctx context.Context, params store.ExpungeParams) (int, error) {
	s.txMu.RLock()
	defer s.txMu.RUnlock()
	if !params.ExpungeDeletedResources && !params.ExpungePreviousVersions && !params.ExpungeEverything {
		params.ExpungeDeletedResources = true
	}
	resTbl, err := s.tenantTableRef(ctx, params.TenantID, "fhir_resource")
	if err != nil {
		return 0, err
	}
	histTbl, err := s.tenantTableRef(ctx, params.TenantID, "fhir_resource_history")
	if err != nil {
		return 0, err
	}
	conds := []string{"tenant_id = ?"}
	args := []any{params.TenantID}
	if params.ResType != "" {
		conds = append(conds, "res_type = ?")
		args = append(args, params.ResType)
	}
	if params.ResID != "" {
		conds = append(conds, "res_id = ?")
		args = append(args, params.ResID)
	}
	q := fmt.Sprintf(`SELECT res_type, res_id, res_version, is_deleted FROM %s WHERE %s`, resTbl, strings.Join(conds, " AND "))
	rows, err := s.queryDorisSQL(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	type targetItem struct {
		resType   string
		resID     string
		version   int
		isDeleted bool
	}
	var targets []targetItem
	for rows.Next() {
		var rt, rid string
		var ver int
		var rawDel any
		if err := rows.Scan(&rt, &rid, &ver, &rawDel); err != nil {
			_ = rows.Close()
			return 0, err
		}
		targets = append(targets, targetItem{rt, rid, ver, parseDorisBool(rawDel)})
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, err
	}
	_ = rows.Close()

	expunged := 0
	for _, it := range targets {
		histCountQ := fmt.Sprintf(`SELECT COUNT(*), COALESCE(MAX(res_version), 0) FROM %s WHERE tenant_id = ? AND res_type = ? AND res_id = ?`, histTbl)
		cRows, err := s.queryDorisSQL(ctx, histCountQ, params.TenantID, it.resType, it.resID)
		if err != nil {
			return expunged, err
		}
		var hCount, maxVer int
		if cRows.Next() {
			_ = cRows.Scan(&hCount, &maxVer)
		}
		_ = cRows.Close()

		if params.ExpungeEverything || (params.ExpungeDeletedResources && it.isDeleted) {
			expunged += 1 + hCount
			_ = s.execDorisSQL(ctx, fmt.Sprintf(`DELETE FROM %s WHERE tenant_id = ? AND res_type = ? AND res_id = ?`, resTbl), params.TenantID, it.resType, it.resID)
			_ = s.execDorisSQL(ctx, fmt.Sprintf(`DELETE FROM %s WHERE tenant_id = ? AND res_type = ? AND res_id = ?`, histTbl), params.TenantID, it.resType, it.resID)
			if tbl, ok := CoreResourceTableName(it.resType); ok {
				coreTbl, _ := s.tenantTableRef(ctx, params.TenantID, tbl)
				_ = s.execDorisSQL(ctx, fmt.Sprintf(`DELETE FROM %s WHERE tenant_id = ? AND res_id = ?`, coreTbl), params.TenantID, it.resID)
			}
			continue
		}
		if params.ExpungePreviousVersions && hCount > 1 {
			expunged += hCount - 1
			_ = s.execDorisSQL(ctx, fmt.Sprintf(`DELETE FROM %s WHERE tenant_id = ? AND res_type = ? AND res_id = ? AND res_version < ?`, histTbl), params.TenantID, it.resType, it.resID, maxVer)
		}
	}
	return expunged, nil
}

func (s *Store) reindexSQL(ctx context.Context, params store.ReindexParams) (*store.ReindexResult, error) {
	s.txMu.RLock()
	defer s.txMu.RUnlock()
	if params.ResType != "" && params.ResID != "" {
		if _, err := s.readResourceWithMetaSQL(ctx, params.TenantID, params.ResType, params.ResID); err != nil {
			return nil, err
		}
	}
	resTbl, err := s.tenantTableRef(ctx, params.TenantID, "fhir_resource")
	if err != nil {
		return nil, err
	}
	conds := []string{"tenant_id = ?", "is_deleted = FALSE"}
	args := []any{params.TenantID}
	if params.ResType != "" {
		conds = append(conds, "res_type = ?")
		args = append(args, params.ResType)
	}
	if params.ResID != "" {
		conds = append(conds, "res_id = ?")
		args = append(args, params.ResID)
	}
	q := fmt.Sprintf(`SELECT %s FROM %s WHERE %s`, dorisResourceSelectCols, resTbl, strings.Join(conds, " AND "))
	rows, err := s.queryDorisSQL(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	extractors := store.DefaultIndexExtractors()
	var entries []storedEntry
	for rows.Next() {
		entry, err := scanDorisStoredEntry(rows, extractors)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()

	out := &store.ReindexResult{
		ByResourceType: make(map[string]int),
	}
	for _, entry := range entries {
		extractor := params.Extractors[entry.row.ResType]
		if extractor == nil {
			continue
		}
		idx, err := extractor(params.TenantID, entry.row.ResID, entry.row.ResourceProto)
		if err != nil {
			return nil, err
		}
		updated := ExtractRow(store.ResourceInput{
			TenantID:       params.TenantID,
			ResType:        entry.row.ResType,
			ResID:          entry.row.ResID,
			IdempotencyKey: entry.row.IdempotencyKey,
			ResourceProto:  entry.row.ResourceProto,
			SearchIndexes:  idx,
		}, entry.row.ResVersion, entry.row.LastUpdated, false)

		stmt, uArgs := buildInsertResourceSQL(resTbl, []DorisResourceRow{updated})
		if err := s.execDorisSQL(ctx, stmt, uArgs...); err != nil {
			return nil, err
		}
		if coreTblName, ok := CoreResourceTableName(updated.ResType); ok {
			coreTbl, err := s.tenantTableRef(ctx, params.TenantID, coreTblName)
			if err != nil {
				return nil, err
			}
			if cStmt, cArgs, ok := ProjectCoreResourceUpsertSQLQualified(coreTbl, updated); ok {
				if err := s.execDorisSQL(ctx, cStmt, cArgs...); err != nil {
					return nil, err
				}
			}
		}
		out.ReindexedResources++
		out.ByResourceType[updated.ResType]++
	}
	return out, nil
}

const dorisAuditSelectCols = `tenant_id, audit_id, recorded, action, subtype_code, outcome, COALESCE(outcome_desc, ''), COALESCE(http_method, ''), COALESCE(http_status, 0), COALESCE(request_uri, ''), COALESCE(agent_subject, ''), COALESCE(agent_patient, ''), COALESCE(client_ip, ''), COALESCE(entity_type, ''), COALESCE(entity_id, ''), COALESCE(entity_version, ''), COALESCE(trace_id, ''), COALESCE(span_id, '')`

func scanAuditRecord(rows *sql.Rows) (store.AuditRecord, error) {
	var rec store.AuditRecord
	var rawRecorded any
	if err := rows.Scan(
		&rec.TenantID, &rec.AuditID, &rawRecorded, &rec.Action, &rec.SubtypeCode, &rec.Outcome,
		&rec.OutcomeDesc, &rec.HTTPMethod, &rec.HTTPStatus, &rec.RequestURI,
		&rec.AgentSubject, &rec.AgentPatient, &rec.ClientIP, &rec.EntityType,
		&rec.EntityID, &rec.EntityVersion, &rec.TraceID, &rec.SpanID,
	); err != nil {
		return store.AuditRecord{}, err
	}
	rec.Recorded = parseDorisTime(rawRecorded)
	store.HydrateAuditOutcomeMetadata(&rec)
	return rec, nil
}

func (s *Store) readAuditEventSQL(ctx context.Context, tenantID, auditID string) (*store.AuditRecord, error) {
	auditTbl, err := s.tenantTableRef(ctx, tenantID, "fhir_audit_event")
	if err != nil {
		return nil, err
	}
	q := fmt.Sprintf(`SELECT %s FROM %s WHERE tenant_id = ? AND audit_id = ? LIMIT 1`, dorisAuditSelectCols, auditTbl)
	rows, err := s.queryDorisSQL(ctx, q, tenantID, auditID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, sql.ErrNoRows
	}
	rec, err := scanAuditRecord(rows)
	if err != nil {
		return nil, err
	}
	return &rec, nil
}

func (s *Store) queryAuditEventsSQL(ctx context.Context, params store.AuditQueryParams) ([]store.AuditRecord, int, error) {
	auditTbl, err := s.tenantTableRef(ctx, params.TenantID, "fhir_audit_event")
	if err != nil {
		return nil, 0, err
	}
	conds := []string{"tenant_id = ?"}
	args := []any{params.TenantID}
	if params.Action != "" {
		conds = append(conds, "action = ?")
		args = append(args, params.Action)
	}
	if params.SubtypeCode != "" {
		conds = append(conds, "subtype_code = ?")
		args = append(args, params.SubtypeCode)
	}
	if params.Outcome != "" {
		conds = append(conds, "outcome = ?")
		args = append(args, params.Outcome)
	}
	if params.AgentSubject != "" {
		conds = append(conds, "agent_subject = ?")
		args = append(args, params.AgentSubject)
	}
	if params.AgentPatient != "" {
		conds = append(conds, "agent_patient = ?")
		args = append(args, params.AgentPatient)
	}
	if params.EntityType != "" {
		conds = append(conds, "entity_type = ?")
		args = append(args, params.EntityType)
	}
	if params.EntityID != "" {
		conds = append(conds, "entity_id = ?")
		args = append(args, params.EntityID)
	}
	whereClause := strings.Join(conds, " AND ")

	cRows, err := s.queryDorisSQL(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE %s`, auditTbl, whereClause), args...)
	if err != nil {
		return nil, 0, err
	}
	var total int
	if cRows.Next() {
		if err := cRows.Scan(&total); err != nil {
			_ = cRows.Close()
			return nil, 0, err
		}
	}
	_ = cRows.Close()

	count := params.Count
	if count <= 0 {
		count = 100
	}
	offset := max(params.Offset, 0)
	if offset >= total || total == 0 {
		return []store.AuditRecord{}, total, nil
	}
	listSQL := fmt.Sprintf(
		`SELECT %s FROM %s WHERE %s ORDER BY recorded DESC, audit_id DESC LIMIT %d OFFSET %d`,
		dorisAuditSelectCols, auditTbl, whereClause, count, offset,
	)
	rows, err := s.queryDorisSQL(ctx, listSQL, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	out := make([]store.AuditRecord, 0, min(count, total-offset))
	for rows.Next() {
		rec, err := scanAuditRecord(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, rec)
	}
	return out, total, rows.Err()
}

func encodeExportJobFields(job store.ExportJobRecord) (typesJSON, countsJSON, filesJSON string) {
	if len(job.TypesOrder) > 0 {
		if b, err := json.Marshal(job.TypesOrder); err == nil {
			typesJSON = string(b)
		}
	}
	if len(job.Counts) > 0 {
		if b, err := json.Marshal(job.Counts); err == nil {
			countsJSON = string(b)
		}
	}
	if len(job.Files) > 0 {
		if b, err := json.Marshal(job.Files); err == nil {
			filesJSON = string(b)
		}
	}
	return typesJSON, countsJSON, filesJSON
}

func decodeExportJobFields(job *store.ExportJobRecord, typesJSON, countsJSON, filesJSON string) {
	if strings.TrimSpace(typesJSON) != "" {
		_ = json.Unmarshal([]byte(typesJSON), &job.TypesOrder)
	}
	if strings.TrimSpace(countsJSON) != "" {
		_ = json.Unmarshal([]byte(countsJSON), &job.Counts)
	}
	if strings.TrimSpace(filesJSON) != "" {
		_ = json.Unmarshal([]byte(filesJSON), &job.Files)
	}
}

func (s *Store) saveExportJobSQL(ctx context.Context, job store.ExportJobRecord) error {
	jobTbl, err := s.tenantTableRef(ctx, job.TenantID, "fhir_export_job")
	if err != nil {
		return err
	}
	typesJSON, countsJSON, filesJSON := encodeExportJobFields(job)
	stmt := fmt.Sprintf(`INSERT INTO %s (tenant_id, job_id, patient_id, status, err_msg, output_format, cancel_requested, request_url, created_at, types_order, counts, files_b64) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, jobTbl)
	return s.execDorisSQL(ctx, stmt, job.TenantID, job.ID, job.PatientID, job.Status, job.Err, job.OutputFormat, job.CancelRequested, job.RequestURL, job.CreatedAt.UTC(), typesJSON, countsJSON, filesJSON)
}

func (s *Store) getExportJobSQL(ctx context.Context, tenantID, jobID string) (*store.ExportJobRecord, error) {
	jobTbl, err := s.tenantTableRef(ctx, tenantID, "fhir_export_job")
	if err != nil {
		return nil, err
	}
	q := fmt.Sprintf(
		`SELECT tenant_id, job_id, COALESCE(patient_id, ''), COALESCE(status, 'complete'), COALESCE(err_msg, ''), COALESCE(output_format, ''), COALESCE(cancel_requested, FALSE), COALESCE(request_url, ''), created_at, COALESCE(types_order, ''), COALESCE(counts, ''), COALESCE(files_b64, '') FROM %s WHERE tenant_id = ? AND job_id = ? LIMIT 1`,
		jobTbl,
	)
	rows, err := s.queryDorisSQL(ctx, q, tenantID, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, sql.ErrNoRows
	}
	var (
		job                              store.ExportJobRecord
		rawCancel, rawCreated            any
		typesJSON, countsJSON, filesJSON string
	)
	if err := rows.Scan(
		&job.TenantID, &job.ID, &job.PatientID, &job.Status, &job.Err, &job.OutputFormat,
		&rawCancel, &job.RequestURL, &rawCreated, &typesJSON, &countsJSON, &filesJSON,
	); err != nil {
		return nil, err
	}
	job.CancelRequested = parseDorisBool(rawCancel)
	job.CreatedAt = parseDorisTime(rawCreated)
	decodeExportJobFields(&job, typesJSON, countsJSON, filesJSON)
	return &job, nil
}

func (s *Store) deleteExportJobSQL(ctx context.Context, tenantID, jobID string) (bool, error) {
	jobTbl, err := s.tenantTableRef(ctx, tenantID, "fhir_export_job")
	if err != nil {
		return false, err
	}
	existed := false
	rows, err := s.queryDorisSQL(ctx, fmt.Sprintf(`SELECT 1 FROM %s WHERE tenant_id = ? AND job_id = ? LIMIT 1`, jobTbl), tenantID, jobID)
	if err != nil {
		return false, err
	}
	existed = rows.Next()
	_ = rows.Close()
	if err := s.execDorisSQL(ctx, fmt.Sprintf(`DELETE FROM %s WHERE tenant_id = ? AND job_id = ?`, jobTbl), tenantID, jobID); err != nil {
		return false, err
	}
	return existed, nil
}

func (s *Store) getOrCreateClusterSecretSQL(ctx context.Context, key string, generate func() ([]byte, error)) ([]byte, error) {
	secretTbl, err := s.clusterTableRef("fhir_cluster_secret")
	if err != nil {
		return nil, err
	}
	readSecret := func() ([]byte, bool, error) {
		rows, qErr := s.queryDorisSQL(ctx, fmt.Sprintf(`SELECT COALESCE(secret_bytes_hex, '') FROM %s WHERE secret_key = ? LIMIT 1`, secretTbl), key)
		if qErr != nil {
			return nil, false, qErr
		}
		defer rows.Close()
		if !rows.Next() {
			return nil, false, rows.Err()
		}
		var hexVal string
		if sErr := rows.Scan(&hexVal); sErr != nil {
			return nil, false, sErr
		}
		b, dErr := hex.DecodeString(strings.TrimSpace(hexVal))
		if dErr != nil || len(b) == 0 {
			return nil, false, nil
		}
		return b, true, nil
	}

	if b, ok, err := readSecret(); err != nil {
		return nil, err
	} else if ok {
		return b, nil
	}
	if generate == nil {
		return nil, sql.ErrNoRows
	}

	rel, err := s.acquireRowLock(ctx, "secret:"+key)
	if err != nil {
		return nil, err
	}
	defer rel()

	if b, ok, err := readSecret(); err != nil {
		return nil, err
	} else if ok {
		return b, nil
	}

	genBytes, err := generate()
	if err != nil {
		return nil, err
	}
	stored := append([]byte(nil), genBytes...)
	if err := s.execDorisSQL(ctx, fmt.Sprintf(`INSERT INTO %s (secret_key, secret_bytes_hex, created_at) VALUES (?, ?, ?)`, secretTbl),
		key, hex.EncodeToString(stored), time.Now().UTC()); err != nil {
		return nil, err
	}
	return stored, nil
}

func (s *Store) readClusterStateRawSQL(ctx context.Context, csTbl, namespace, key string) (store.ClusterStateRecord, bool, error) {
	q := fmt.Sprintf(
		`SELECT tenant_id, secondary_key, value_hex, version, expires_at, updated_at FROM %s WHERE namespace = ? AND state_key = ? LIMIT 1`,
		csTbl,
	)
	rows, err := s.queryDorisSQL(ctx, q, namespace, key)
	if err != nil {
		return store.ClusterStateRecord{}, false, err
	}
	defer rows.Close()
	if !rows.Next() {
		return store.ClusterStateRecord{}, false, rows.Err()
	}
	var (
		rec                    store.ClusterStateRecord
		valueHex               string
		rawExpires, rawUpdated any
	)
	rec.Namespace = namespace
	rec.Key = key
	if err := rows.Scan(&rec.TenantID, &rec.SecondaryKey, &valueHex, &rec.Version, &rawExpires, &rawUpdated); err != nil {
		return store.ClusterStateRecord{}, false, err
	}
	valBytes, _ := hex.DecodeString(strings.TrimSpace(valueHex))
	if valBytes == nil {
		valBytes = []byte{}
	}
	rec.Value = valBytes
	if rawExpires != nil {
		if t := parseDorisTime(rawExpires); !t.IsZero() {
			rec.ExpiresAt = &t
		}
	}
	rec.UpdatedAt = parseDorisTime(rawUpdated)
	return rec, true, nil
}

func (s *Store) putClusterStateSQL(ctx context.Context, rec store.ClusterStateRecord) (store.ClusterStateRecord, error) {
	rel, err := s.acquireRowLock(ctx, "cs:"+rec.Namespace+":"+rec.Key)
	if err != nil {
		return store.ClusterStateRecord{}, err
	}
	defer rel()

	csTbl, err := s.clusterTableRef("fhir_cluster_state")
	if err != nil {
		return store.ClusterStateRecord{}, err
	}
	existing, exists, err := s.readClusterStateRawSQL(ctx, csTbl, rec.Namespace, rec.Key)
	if err != nil {
		return store.ClusterStateRecord{}, err
	}
	ver := int64(1)
	if exists {
		ver = existing.Version + 1
	}
	stored := cloneDorisClusterState(rec)
	if stored.Value == nil {
		stored.Value = []byte{}
	}
	stored.Version = ver
	stored.UpdatedAt = time.Now().UTC()
	if err := s.execDorisSQL(ctx, fmt.Sprintf(`INSERT INTO %s (namespace, state_key, tenant_id, secondary_key, value_hex, version, expires_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, csTbl),
		stored.Namespace, stored.Key, stored.TenantID, stored.SecondaryKey, hex.EncodeToString(stored.Value), stored.Version, nullableTimeArg(stored.ExpiresAt), stored.UpdatedAt); err != nil {
		return store.ClusterStateRecord{}, err
	}
	return cloneDorisClusterState(stored), nil
}

func (s *Store) getClusterStateSQL(ctx context.Context, namespace, key string) (*store.ClusterStateRecord, bool, error) {
	csTbl, err := s.clusterTableRef("fhir_cluster_state")
	if err != nil {
		return nil, false, err
	}
	rec, ok, err := s.readClusterStateRawSQL(ctx, csTbl, namespace, key)
	if err != nil || !ok {
		return nil, false, err
	}
	if isDorisClusterStateExpired(rec, time.Now().UTC()) {
		return nil, false, nil
	}
	cp := cloneDorisClusterState(rec)
	return &cp, true, nil
}

func (s *Store) getClusterStateBySecondarySQL(ctx context.Context, namespace, secondaryKey string) (*store.ClusterStateRecord, bool, error) {
	csTbl, err := s.clusterTableRef("fhir_cluster_state")
	if err != nil {
		return nil, false, err
	}
	q := fmt.Sprintf(
		`SELECT state_key, tenant_id, value_hex, version, expires_at, updated_at FROM %s WHERE namespace = ? AND secondary_key = ?`,
		csTbl,
	)
	rows, err := s.queryDorisSQL(ctx, q, namespace, secondaryKey)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	now := time.Now().UTC()
	var best *store.ClusterStateRecord
	for rows.Next() {
		var (
			rec                    store.ClusterStateRecord
			valueHex               string
			rawExpires, rawUpdated any
		)
		rec.Namespace = namespace
		rec.SecondaryKey = secondaryKey
		if err := rows.Scan(&rec.Key, &rec.TenantID, &valueHex, &rec.Version, &rawExpires, &rawUpdated); err != nil {
			return nil, false, err
		}
		valBytes, _ := hex.DecodeString(strings.TrimSpace(valueHex))
		if valBytes == nil {
			valBytes = []byte{}
		}
		rec.Value = valBytes
		if rawExpires != nil {
			if t := parseDorisTime(rawExpires); !t.IsZero() {
				rec.ExpiresAt = &t
			}
		}
		rec.UpdatedAt = parseDorisTime(rawUpdated)
		if isDorisClusterStateExpired(rec, now) {
			continue
		}
		if best == nil || rec.UpdatedAt.After(best.UpdatedAt) || (rec.UpdatedAt.Equal(best.UpdatedAt) && rec.Key < best.Key) {
			cp := cloneDorisClusterState(rec)
			best = &cp
		}
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if best == nil {
		return nil, false, nil
	}
	return best, true, nil
}

func (s *Store) compareAndSwapClusterStateSQL(ctx context.Context, expectedVersion int64, rec store.ClusterStateRecord) (store.ClusterStateRecord, bool, error) {
	rel, err := s.acquireRowLock(ctx, "cs:"+rec.Namespace+":"+rec.Key)
	if err != nil {
		return store.ClusterStateRecord{}, false, err
	}
	defer rel()

	csTbl, err := s.clusterTableRef("fhir_cluster_state")
	if err != nil {
		return store.ClusterStateRecord{}, false, err
	}
	now := time.Now().UTC()
	existing, exists, err := s.readClusterStateRawSQL(ctx, csTbl, rec.Namespace, rec.Key)
	if err != nil {
		return store.ClusterStateRecord{}, false, err
	}
	if exists && isDorisClusterStateExpired(existing, now) {
		exists = false
	}
	if expectedVersion == 0 {
		if exists {
			return store.ClusterStateRecord{}, false, nil
		}
	} else {
		if !exists || existing.Version != expectedVersion {
			return store.ClusterStateRecord{}, false, nil
		}
	}

	stored := cloneDorisClusterState(rec)
	if stored.Value == nil {
		stored.Value = []byte{}
	}
	stored.Version = expectedVersion + 1
	stored.UpdatedAt = now
	if err := s.execDorisSQL(ctx, fmt.Sprintf(`INSERT INTO %s (namespace, state_key, tenant_id, secondary_key, value_hex, version, expires_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, csTbl),
		stored.Namespace, stored.Key, stored.TenantID, stored.SecondaryKey, hex.EncodeToString(stored.Value), stored.Version, nullableTimeArg(stored.ExpiresAt), stored.UpdatedAt); err != nil {
		return store.ClusterStateRecord{}, false, err
	}
	return cloneDorisClusterState(stored), true, nil
}

func (s *Store) consumeClusterStateSQL(ctx context.Context, namespace, key string) (*store.ClusterStateRecord, bool, error) {
	rel, err := s.acquireRowLock(ctx, "cs:"+namespace+":"+key)
	if err != nil {
		return nil, false, err
	}
	defer rel()

	csTbl, err := s.clusterTableRef("fhir_cluster_state")
	if err != nil {
		return nil, false, err
	}
	rec, ok, err := s.readClusterStateRawSQL(ctx, csTbl, namespace, key)
	if err != nil || !ok {
		return nil, false, err
	}
	if err := s.execDorisSQL(ctx, fmt.Sprintf(`DELETE FROM %s WHERE namespace = ? AND state_key = ?`, csTbl), namespace, key); err != nil {
		return nil, false, err
	}
	if isDorisClusterStateExpired(rec, time.Now().UTC()) {
		return nil, false, nil
	}
	cp := cloneDorisClusterState(rec)
	return &cp, true, nil
}

func (s *Store) deleteClusterStateSQL(ctx context.Context, namespace, key string) (bool, error) {
	csTbl, err := s.clusterTableRef("fhir_cluster_state")
	if err != nil {
		return false, err
	}
	_, ok, err := s.readClusterStateRawSQL(ctx, csTbl, namespace, key)
	if err != nil {
		return false, err
	}
	if err := s.execDorisSQL(ctx, fmt.Sprintf(`DELETE FROM %s WHERE namespace = ? AND state_key = ?`, csTbl), namespace, key); err != nil {
		return false, err
	}
	return ok, nil
}

func (s *Store) listClusterStatesSQL(ctx context.Context, namespace, tenantID string) ([]store.ClusterStateRecord, error) {
	csTbl, err := s.clusterTableRef("fhir_cluster_state")
	if err != nil {
		return nil, err
	}
	conds := []string{"namespace = ?"}
	args := []any{namespace}
	if tenantID != "" {
		conds = append(conds, "tenant_id = ?")
		args = append(args, tenantID)
	}
	q := fmt.Sprintf(
		`SELECT state_key, tenant_id, secondary_key, value_hex, version, expires_at, updated_at FROM %s WHERE %s ORDER BY state_key ASC`,
		csTbl, strings.Join(conds, " AND "),
	)
	rows, err := s.queryDorisSQL(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	now := time.Now().UTC()
	var out []store.ClusterStateRecord
	for rows.Next() {
		var (
			rec                    store.ClusterStateRecord
			valueHex               string
			rawExpires, rawUpdated any
		)
		rec.Namespace = namespace
		if err := rows.Scan(&rec.Key, &rec.TenantID, &rec.SecondaryKey, &valueHex, &rec.Version, &rawExpires, &rawUpdated); err != nil {
			return nil, err
		}
		valBytes, _ := hex.DecodeString(strings.TrimSpace(valueHex))
		if valBytes == nil {
			valBytes = []byte{}
		}
		rec.Value = valBytes
		if rawExpires != nil {
			if t := parseDorisTime(rawExpires); !t.IsZero() {
				rec.ExpiresAt = &t
			}
		}
		rec.UpdatedAt = parseDorisTime(rawUpdated)
		if isDorisClusterStateExpired(rec, now) {
			continue
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (s *Store) saveAuthCodeSQL(ctx context.Context, rawCode string, rec store.OAuthAuthCodeRecord) error {
	payload, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	_, err = s.putClusterStateSQL(ctx, store.ClusterStateRecord{
		Namespace: dorisNSOAuthAuthCode,
		Key:       hashToken(rawCode),
		TenantID:  rec.TenantID,
		Value:     payload,
		ExpiresAt: nullablePtrTime(rec.ExpiresAt),
	})
	return err
}

func (s *Store) consumeAuthCodeSQL(ctx context.Context, rawCode string) (store.OAuthAuthCodeRecord, bool, error) {
	cs, ok, err := s.consumeClusterStateSQL(ctx, dorisNSOAuthAuthCode, hashToken(rawCode))
	if err != nil || !ok || cs == nil {
		return store.OAuthAuthCodeRecord{}, false, err
	}
	var rec store.OAuthAuthCodeRecord
	if err := json.Unmarshal(cs.Value, &rec); err != nil {
		return store.OAuthAuthCodeRecord{}, false, err
	}
	if !rec.ExpiresAt.IsZero() && time.Now().UTC().After(rec.ExpiresAt) {
		return store.OAuthAuthCodeRecord{}, false, nil
	}
	return rec, true, nil
}

func (s *Store) saveRefreshTokenSQL(ctx context.Context, rawToken string, rec store.OAuthRefreshTokenRecord) error {
	payload, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	_, err = s.putClusterStateSQL(ctx, store.ClusterStateRecord{
		Namespace: dorisNSOAuthRefresh,
		Key:       hashToken(rawToken),
		TenantID:  rec.TenantID,
		Value:     payload,
		ExpiresAt: nullablePtrTime(rec.ExpiresAt),
	})
	return err
}

func (s *Store) peekRefreshTokenSQL(ctx context.Context, rawToken string) (store.OAuthRefreshTokenRecord, bool, error) {
	cs, ok, err := s.getClusterStateSQL(ctx, dorisNSOAuthRefresh, hashToken(rawToken))
	if err != nil || !ok || cs == nil {
		return store.OAuthRefreshTokenRecord{}, false, err
	}
	var rec store.OAuthRefreshTokenRecord
	if err := json.Unmarshal(cs.Value, &rec); err != nil {
		return store.OAuthRefreshTokenRecord{}, false, err
	}
	if !rec.ExpiresAt.IsZero() && time.Now().UTC().After(rec.ExpiresAt) {
		return store.OAuthRefreshTokenRecord{}, false, nil
	}
	return rec, true, nil
}

func (s *Store) consumeRefreshTokenSQL(ctx context.Context, rawToken string) (store.OAuthRefreshTokenRecord, bool, error) {
	cs, ok, err := s.consumeClusterStateSQL(ctx, dorisNSOAuthRefresh, hashToken(rawToken))
	if err != nil || !ok || cs == nil {
		return store.OAuthRefreshTokenRecord{}, false, err
	}
	var rec store.OAuthRefreshTokenRecord
	if err := json.Unmarshal(cs.Value, &rec); err != nil {
		return store.OAuthRefreshTokenRecord{}, false, err
	}
	if !rec.ExpiresAt.IsZero() && time.Now().UTC().After(rec.ExpiresAt) {
		return store.OAuthRefreshTokenRecord{}, false, nil
	}
	return rec, true, nil
}

func (s *Store) revokeTokenSQL(ctx context.Context, rawToken string, ttl time.Duration) error {
	h := hashToken(rawToken)
	if _, err := s.deleteClusterStateSQL(ctx, dorisNSOAuthRefresh, h); err != nil {
		return err
	}
	exp := time.Now().UTC().Add(ttl)
	_, err := s.putClusterStateSQL(ctx, store.ClusterStateRecord{
		Namespace: dorisNSOAuthRevoked,
		Key:       h,
		Value:     []byte{1},
		ExpiresAt: &exp,
	})
	return err
}

func (s *Store) isTokenRevokedSQL(ctx context.Context, rawToken string) (bool, error) {
	_, ok, err := s.getClusterStateSQL(ctx, dorisNSOAuthRevoked, hashToken(rawToken))
	return ok, err
}

func (s *Store) claimNonceSQL(ctx context.Context, nonceKey string, ttl time.Duration) (bool, error) {
	exp := time.Now().UTC().Add(ttl)
	_, swapped, err := s.compareAndSwapClusterStateSQL(ctx, 0, store.ClusterStateRecord{
		Namespace: dorisNSReplayNonce,
		Key:       hashToken("nonce:" + nonceKey),
		Value:     []byte{1},
		ExpiresAt: &exp,
	})
	return swapped, err
}
