package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/flint-fhir/flint/store"
	"github.com/flint-fhir/flint/store/doris"
	"github.com/flint-fhir/flint/store/postgres"
)

func TestParseBackendType(t *testing.T) {
	b, err := store.ParseBackendType("")
	require.NoError(t, err)
	assert.Equal(t, store.BackendPostgres, b)

	for _, raw := range []string{"postgres", "PostgreSQL", "pg"} {
		b, err = store.ParseBackendType(raw)
		require.NoError(t, err)
		assert.Equal(t, store.BackendPostgres, b)
	}

	for _, raw := range []string{"doris", "Apache-Doris", "DORIS"} {
		b, err = store.ParseBackendType(raw)
		require.NoError(t, err)
		assert.Equal(t, store.BackendDoris, b)
	}

	for _, raw := range []string{"sqlite", "SQLite3", "SQLITE"} {
		b, err = store.ParseBackendType(raw)
		require.NoError(t, err)
		assert.Equal(t, store.BackendSQLite, b)
	}

	_, err = store.ParseBackendType("mysql")
	require.ErrorContains(t, err, "invalid store backend")
}

func TestParseOptionalBackendType(t *testing.T) {
	for _, raw := range []string{"", "none", "off", "disabled", "false"} {
		b, err := store.ParseOptionalBackendType(raw)
		require.NoError(t, err)
		assert.Equal(t, store.BackendType(""), b)
	}

	b, err := store.ParseOptionalBackendType("doris")
	require.NoError(t, err)
	assert.Equal(t, store.BackendDoris, b)

	b, err = store.ParseOptionalBackendType("postgres")
	require.NoError(t, err)
	assert.Equal(t, store.BackendPostgres, b)

	b, err = store.ParseOptionalBackendType("sqlite")
	require.NoError(t, err)
	assert.Equal(t, store.BackendSQLite, b)

	_, err = store.ParseOptionalBackendType("oracle")
	require.ErrorContains(t, err, "invalid analytics backend")
}

func TestIsNil(t *testing.T) {
	assert.True(t, store.IsNil(nil))

	var typedNilPG *postgres.Store
	var ifacePG store.Store = typedNilPG
	assert.True(t, store.IsNil(ifacePG))

	var typedNilDoris *doris.Store
	var ifaceDoris store.Store = typedNilDoris
	assert.True(t, store.IsNil(ifaceDoris))

	ds := doris.New(nil)
	assert.False(t, store.IsNil(ds))
}
