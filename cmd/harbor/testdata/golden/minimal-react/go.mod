module github.com/example/acme-agent

go 1.26

require github.com/hurtener/Harbor v1.26.11

// Harbor resolves from the module proxy — `go mod tidy && go build ./...`
// works with no edit to this file.
//
// Building against a LOCAL Harbor checkout instead (contributors, or
// testing an unreleased change)? Uncomment the directive below and point
// it at your clone:
//
//	replace github.com/hurtener/Harbor => ../Harbor
//
// Adjust the relative path if your Harbor clone lives elsewhere.
