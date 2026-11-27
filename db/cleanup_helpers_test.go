package db

import "testing"

func cleanupErr(t *testing.T, what string, err error) {
	t.Helper()
	if err != nil {
		t.Errorf("cleanup %s: %v", what, err)
	}
}

func cleanupExec(t *testing.T, d *DB, query string, args ...any) {
	t.Helper()
	_, err := d.Exec(query, args...)
	cleanupErr(t, query, err)
}
