package catalog

// StubRefreshForTest replaces the catalog refresh used by the self-healing
// resolvers and returns a restore func. For tests in other packages only.
func StubRefreshForTest(f func() error) (restore func()) {
	prev := catalogRefresh
	catalogRefresh = f
	return func() { catalogRefresh = prev }
}
