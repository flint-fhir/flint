package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/flint-fhir/flint/store"
)

type stubStore struct {
	store.Store
}

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

	var typedNil *stubStore
	var iface store.Store = typedNil
	assert.True(t, store.IsNil(iface))

	nonNil := &stubStore{}
	assert.False(t, store.IsNil(nonNil))
}
