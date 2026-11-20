package doris

import (
	"database/sql"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

const (
	defaultConnectTimeout  = 10 * time.Second
	defaultReadTimeout     = 30 * time.Second
	defaultWriteTimeout    = 30 * time.Second
	defaultMaxOpenConns    = 25
	defaultMaxIdleConns    = 10
	defaultConnMaxLifetime = 5 * time.Minute
	defaultConnMaxIdleTime = 1 * time.Minute
)

var dorisSessionVarIdentRegex = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// DSNOptions controls normalization and hardening of Apache Doris MySQL-protocol DSNs.
type DSNOptions struct {
	DefaultDatabase string
	GroupCommit     GroupCommitMode
	WorkloadGroup   string
	ComputeGroup    string
	RequireNonRoot  bool
	ConnectTimeout  time.Duration
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	TLSConfig       string
}

// NormalizeDSN parses and normalizes an Apache Doris MySQL DSN, enforcing single-RTT
// parameter interpolation (`interpolateParams=true`), UTC time parsing (`parseTime=true`),
// connection/I/O timeouts, optional non-root user enforcement, and handshake `group_commit`.
func NormalizeDSN(rawDSN string, opts DSNOptions) (string, error) {
	trimmed := strings.TrimSpace(rawDSN)
	if trimmed == "" {
		return "", fmt.Errorf("doris dsn must not be empty")
	}
	cfg, err := mysql.ParseDSN(trimmed)
	if err != nil {
		return "", fmt.Errorf("parse doris dsn: %w", err)
	}
	if opts.RequireNonRoot {
		userLower := strings.ToLower(strings.TrimSpace(cfg.User))
		if userLower == "" || userLower == "root" || userLower == "admin" {
			return "", fmt.Errorf("doris dsn user %q rejected: non-root principal required", cfg.User)
		}
	}

	cfg.ParseTime = true
	cfg.Loc = time.UTC
	cfg.InterpolateParams = true

	if cfg.Timeout <= 0 {
		if opts.ConnectTimeout > 0 {
			cfg.Timeout = opts.ConnectTimeout
		} else {
			cfg.Timeout = defaultConnectTimeout
		}
	}
	if cfg.ReadTimeout <= 0 {
		if opts.ReadTimeout > 0 {
			cfg.ReadTimeout = opts.ReadTimeout
		} else {
			cfg.ReadTimeout = defaultReadTimeout
		}
	}
	if cfg.WriteTimeout <= 0 {
		if opts.WriteTimeout > 0 {
			cfg.WriteTimeout = opts.WriteTimeout
		} else {
			cfg.WriteTimeout = defaultWriteTimeout
		}
	}

	if strings.TrimSpace(cfg.DBName) == "" {
		if defDB := strings.TrimSpace(opts.DefaultDatabase); defDB != "" {
			if err := ValidateIdentifier(defDB); err != nil {
				return "", err
			}
			cfg.DBName = defDB
		}
	} else if err := ValidateIdentifier(cfg.DBName); err != nil {
		return "", err
	}

	if tlsName := strings.TrimSpace(opts.TLSConfig); tlsName != "" && strings.TrimSpace(cfg.TLSConfig) == "" {
		cfg.TLSConfig = tlsName
	}

	if cfg.Params == nil {
		cfg.Params = make(map[string]string)
	}
	if opts.GroupCommit != "" {
		gc, err := ParseGroupCommitMode(string(opts.GroupCommit))
		if err != nil {
			return "", err
		}
		cfg.Params["group_commit"] = "'" + string(gc) + "'"
	}
	if wg := strings.TrimSpace(opts.WorkloadGroup); wg != "" {
		if !dorisSessionVarIdentRegex.MatchString(wg) {
			return "", fmt.Errorf("invalid doris workload_group %q", opts.WorkloadGroup)
		}
		cfg.Params["workload_group"] = "'" + wg + "'"
	}
	if cg := strings.TrimSpace(opts.ComputeGroup); cg != "" {
		if !dorisSessionVarIdentRegex.MatchString(cg) {
			return "", fmt.Errorf("invalid doris compute_group %q", opts.ComputeGroup)
		}
		cfg.Params["compute_group"] = "'" + cg + "'"
	}

	return cfg.FormatDSN(), nil
}

// ConfigurePool applies production connection pool limits to a Doris *sql.DB handle.
func ConfigurePool(db *sql.DB, maxOpen, maxIdle int, maxLifetime, maxIdleTime time.Duration) {
	if db == nil {
		return
	}
	if maxOpen <= 0 {
		maxOpen = defaultMaxOpenConns
	}
	if maxIdle <= 0 {
		maxIdle = defaultMaxIdleConns
	}
	if maxIdle > maxOpen {
		maxIdle = maxOpen
	}
	if maxLifetime <= 0 {
		maxLifetime = defaultConnMaxLifetime
	}
	if maxIdleTime <= 0 {
		maxIdleTime = defaultConnMaxIdleTime
	}
	db.SetMaxOpenConns(maxOpen)
	db.SetMaxIdleConns(maxIdle)
	db.SetConnMaxLifetime(maxLifetime)
	db.SetConnMaxIdleTime(maxIdleTime)
}

// ResourceLimits specifies statement-level Apache Doris query governance applied via
// `SELECT /*+ SET_VAR(...) */` optimizer hints.
type ResourceLimits struct {
	WorkloadGroup   string
	ComputeGroup    string
	ExecMemLimit    int64
	CPUThreads      int
	QueryTimeoutSec int
}

// Validate checks that string session variable identifiers are safe against injection
// and numeric limits are non-negative.
func (r ResourceLimits) Validate() error {
	if wg := strings.TrimSpace(r.WorkloadGroup); wg != "" && !dorisSessionVarIdentRegex.MatchString(wg) {
		return fmt.Errorf("invalid doris workload_group %q: must match ^[a-zA-Z0-9_-]{1,64}$", r.WorkloadGroup)
	}
	if cg := strings.TrimSpace(r.ComputeGroup); cg != "" && !dorisSessionVarIdentRegex.MatchString(cg) {
		return fmt.Errorf("invalid doris compute_group %q: must match ^[a-zA-Z0-9_-]{1,64}$", r.ComputeGroup)
	}
	if r.ExecMemLimit < 0 {
		return fmt.Errorf("invalid doris exec_mem_limit %d: must be >= 0", r.ExecMemLimit)
	}
	if r.CPUThreads < 0 {
		return fmt.Errorf("invalid doris cpu_threads %d: must be >= 0", r.CPUThreads)
	}
	if r.QueryTimeoutSec < 0 {
		return fmt.Errorf("invalid doris query_timeout %d: must be >= 0", r.QueryTimeoutSec)
	}
	return nil
}

// FormatSetVarHint formats a safe Doris `/*+ SET_VAR(...) */` optimizer hint or "" when empty/invalid.
func (r ResourceLimits) FormatSetVarHint() string {
	if err := r.Validate(); err != nil {
		return ""
	}
	var parts []string
	if wg := strings.TrimSpace(r.WorkloadGroup); wg != "" {
		parts = append(parts, fmt.Sprintf("workload_group='%s'", wg))
	}
	if cg := strings.TrimSpace(r.ComputeGroup); cg != "" {
		parts = append(parts, fmt.Sprintf("compute_group='%s'", cg))
	}
	if r.ExecMemLimit > 0 {
		parts = append(parts, fmt.Sprintf("exec_mem_limit=%d", r.ExecMemLimit))
	}
	if r.CPUThreads > 0 {
		parts = append(parts, fmt.Sprintf("parallel_pipeline_task_num=%d", r.CPUThreads))
	}
	if r.QueryTimeoutSec > 0 {
		parts = append(parts, fmt.Sprintf("query_timeout=%d", r.QueryTimeoutSec))
	}
	if len(parts) == 0 {
		return ""
	}
	return "/*+ SET_VAR(" + strings.Join(parts, ", ") + ") */"
}

// InjectSelectSetVarHint injects hint immediately after the leading SELECT keyword.
// Non-SELECT statements (INSERT, DELETE, CREATE, SET) are returned unmodified because
// Doris optimizer hints apply only to query statements.
func InjectSelectSetVarHint(query, hint string) string {
	cleanHint := strings.TrimSpace(hint)
	if cleanHint == "" {
		return query
	}
	trimmed := strings.TrimLeft(query, " \t\r\n")
	if len(trimmed) < 7 || !strings.EqualFold(trimmed[:6], "SELECT") {
		return query
	}
	switch trimmed[6] {
	case ' ', '\t', '\r', '\n':
	default:
		return query
	}
	if strings.Contains(trimmed, "/*+") {
		return query
	}
	leadingWS := query[:len(query)-len(trimmed)]
	rest := strings.TrimLeft(trimmed[6:], " \t\r\n")
	return leadingWS + trimmed[:6] + " " + cleanHint + " " + rest
}
