package db

// skipper is the part of testing.TB used by PostgreSQL test helpers.
type skipper interface {
	Helper()
	Skip(args ...any)
	Skipf(format string, args ...any)
}

// SkipPGUnavailable skips when the test database could not be configured.
func SkipPGUnavailable(t skipper, err error) {
	t.Helper()
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
}
