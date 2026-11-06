package plugin

import (
	"encoding/json"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

// Envelope helpers for the host RPC wire contract. Every handler result
// travels in a success envelope; every failure travels as a typed error
// envelope. Results are returned IN the envelope, never as Go errors, so a
// handler bug can never be mistaken by the C glue for a fatal process error.

// ok wraps result in a success envelope.
func ok(result any) []byte {
	raw, _ := json.Marshal(result)
	out, _ := json.Marshal(pluginabi.Envelope{OK: true, Result: raw})
	return out
}

// errEnvelope wraps a typed plugin error in a failure envelope. The Error's
// HTTPStatus (when set) is what the host surfaces to the client; Retryable
// drives host-side retry decisions. Code/Message/HTTPStatus/Retryable are all
// part of the typed error contract.
func errEnvelope(e *pluginabi.Error) []byte {
	out, _ := json.Marshal(pluginabi.Envelope{OK: false, Error: e})
	return out
}

// typedErr is a convenience over pluginabi.NewError so call sites stay short.
func typedErr(code, message string, httpStatus ...int) *pluginabi.Error {
	return pluginabi.NewError(code, message, httpStatus...)
}

// ErrEnvelope is the exported form of errEnvelope for package main (the C
// glue builds envelopes when the method name itself is missing or oversized,
// outside any Manager handler). httpStatus 0 keeps the host default.
func ErrEnvelope(code, message string, httpStatus ...int) []byte {
	return errEnvelope(typedErr(code, message, httpStatus...))
}
