// Package loadsmoke loads the built EasyCLI/CLIProxyAPI v8 plugin shared
// object through the same C ABI the host uses and drives it with an offline
// fake host. It exists so the artifact the CI build produces is exercised, not
// only the in-process Manager.
package loadsmoke
