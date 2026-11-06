// Package tests holds the operator-gated live verification for the
// chatgpt-proxy plugin. Every file here is build-tagged so the default
// `go test ./...` suite stays offline: nothing in this package touches the
// network without -tags live and a live credential from the operator.
package tests
