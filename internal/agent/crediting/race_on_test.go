//go:build race

package crediting

// propertySeeds is how many seeds the finality property test runs. The race
// detector makes each seed about ten times slower, so race builds run
// fewer; plain `go test` (and the CI agent matrix) runs them all.
const propertySeeds = 2
