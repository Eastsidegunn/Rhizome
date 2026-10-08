package trust

// AuthorityForTest returns the package's non-zero authority solely to the
// external tests compiled as part of the trust package's test binary.
func AuthorityForTest() Authority { return newAuthority() }
