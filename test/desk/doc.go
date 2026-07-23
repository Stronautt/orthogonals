// Package desk holds the read-only tests that run against the developer's own
// hardware. It carries no non-test code; see desk_test.go, which is behind the
// `desk` build tag so `go test ./...` never reaches for a real machine.
package desk
