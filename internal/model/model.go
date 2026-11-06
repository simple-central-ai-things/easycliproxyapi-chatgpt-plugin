// Package model holds the shared domain types both plugin layers depend on:
// the credential record (auth-file payload) and the validated host config.
//
// It is a leaf package (imports no other plugin package) because the
// dependency direction is fixed: internal/plugin imports internal/upstream
// (quota cards, the model catalog, the executor) and internal/upstream
// needs the credential/config types to build headers and transports.
// Extracting them here breaks the cycle; internal/plugin keeps type aliases
// and thin wrappers so every existing plugin-side reference is unchanged.
package model
