package upstream

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

type turnstileVector struct {
	Name             string
	P                string
	DX               string
	ExpectedToken    *string // nil = the oracle returned None
	OracleNull       bool
	GoError          string // expected SentinelError code for oracle-null vectors
	ExpectedOmapAdds [][]string
	// ExpectResult marks a vector whose token is NOT deterministic (live
	// challenges embed wall-clock/RNG-derived fingerprint floats): the VM
	// must still solve it. Pairs with ExpectedPasses (the multi-stage
	// chain) and ExpectedPlainPrefix/PlainXORKey (the token's stable
	// plaintext head).
	ExpectedPlainPrefix string
	PlainXORKey         string
	ExpectResult        bool
	ExpectedPasses      []int
}

func loadTurnstileVectors(t *testing.T) []turnstileVector {
	t.Helper()
	var out []turnstileVector
	for _, obj := range fixtureObjects(t, "turnstile") {
		v := turnstileVector{
			Name:       fixtureString(t, obj, "name"),
			P:          fixtureString(t, obj, "p"),
			DX:         fixtureString(t, obj, "dx"),
			OracleNull: fixtureField(t, obj, "oracle_null").(bool),
		}
		if tok, ok := fixtureFieldOpt(obj, "expected_token"); ok && tok != nil {
			s := tok.(string)
			v.ExpectedToken = &s
		}
		if g, ok := fixtureFieldOpt(obj, "go_error"); ok {
			v.GoError = g.(string)
		}
		if adds, ok := fixtureFieldOpt(obj, "expected_omap_adds"); ok {
			for _, pair := range adds.([]any) {
				entry := pair.([]any)
				v.ExpectedOmapAdds = append(v.ExpectedOmapAdds, []string{entry[0].(string), entry[1].(string)})
			}
		}
		if er, ok := fixtureFieldOpt(obj, "expect_result"); ok {
			v.ExpectResult = er.(bool)
		}
		if passes, ok := fixtureFieldOpt(obj, "expected_passes"); ok {
			for _, n := range passes.([]any) {
				switch num := n.(type) {
				case int:
					v.ExpectedPasses = append(v.ExpectedPasses, num)
				case float64:
					v.ExpectedPasses = append(v.ExpectedPasses, int(num))
				}
			}
		}
		if prefix, ok := fixtureFieldOpt(obj, "expected_plain_prefix"); ok {
			v.ExpectedPlainPrefix = prefix.(string)
		}
		if key, ok := fixtureFieldOpt(obj, "plain_xor_key"); ok {
			v.PlainXORKey = key.(string)
		}
		out = append(out, v)
	}
	return out
}

// TestTurnstileOpcodes runs every positive fixture vector through the Go VM.
// Vectors with an expected_token require the byte-identical token (the
// synthetic programs are fully deterministic). Live-challenge vectors
// (expect_result) embed wall-clock/RNG-derived fingerprint floats, so only
// the solve outcome, the multi-stage pass chain and the token's stable
// plaintext head are asserted — see testdata/turnstile/_note.txt.
func TestTurnstileOpcodes(t *testing.T) {
	for _, v := range loadTurnstileVectors(t) {
		if v.OracleNull {
			continue
		}
		t.Run(v.Name, func(t *testing.T) {
			vm, err := solveTurnstileVM(v.DX, v.P)
			if err != nil {
				t.Fatalf("solveTurnstileVM: %v", err)
			}
			if v.ExpectResult {
				if vm.result == "" {
					t.Fatal("turnstile VM produced no token for an expect_result vector")
				}
				if len(v.ExpectedPasses) > 0 {
					if len(vm.passes) != len(v.ExpectedPasses) {
						t.Fatalf("pass chain = %v, want %v", vm.passes, v.ExpectedPasses)
					}
					for i := range vm.passes {
						if vm.passes[i] != v.ExpectedPasses[i] {
							t.Fatalf("pass chain = %v, want %v", vm.passes, v.ExpectedPasses)
						}
					}
				}
				if v.ExpectedPlainPrefix != "" {
					inner, decErr := base64.StdEncoding.DecodeString(vm.result)
					if decErr != nil {
						t.Fatalf("token is not base64: %v", decErr)
					}
					plain := make([]byte, len(inner))
					keyRunes := []rune(v.PlainXORKey)
					for i, b := range inner {
						plain[i] = byte(rune(b) ^ keyRunes[i%len(keyRunes)])
					}
					if len(plain) < len(v.ExpectedPlainPrefix) ||
						string(plain[:len(v.ExpectedPlainPrefix)]) != v.ExpectedPlainPrefix {
						t.Fatalf("token plaintext head mismatch:\n got: %q\nwant: %q",
							string(plain[:min(len(plain), len(v.ExpectedPlainPrefix)+8)]), v.ExpectedPlainPrefix)
					}
				}
				// The public wrapper returns the same token with no error.
				token, err := SolveTurnstile(v.DX, v.P)
				if err != nil {
					t.Fatalf("SolveTurnstile: %v", err)
				}
				if token == "" {
					t.Fatal("SolveTurnstile returned an empty token")
				}
				return
			}
			if vm.result != *v.ExpectedToken {
				t.Fatalf("turnstile token mismatch:\n got: %s\nwant: %s", vm.result, *v.ExpectedToken)
			}
			// The public wrapper returns the same token with no error.
			token, err := SolveTurnstile(v.DX, v.P)
			if err != nil {
				t.Fatalf("SolveTurnstile: %v", err)
			}
			if token != *v.ExpectedToken {
				t.Fatalf("SolveTurnstile token = %q, want %q", token, *v.ExpectedToken)
			}
			if len(v.ExpectedOmapAdds) > 0 {
				got := make([][]string, 0, len(vm.omapAdds))
				for _, op := range vm.omapAdds {
					got = append(got, []string{op[0].(string), op[1].(string)})
				}
				if len(got) != len(v.ExpectedOmapAdds) {
					t.Fatalf("OrderedMap.add calls = %v, want %v", got, v.ExpectedOmapAdds)
				}
				for i := range got {
					if got[i][0] != v.ExpectedOmapAdds[i][0] || got[i][1] != v.ExpectedOmapAdds[i][1] {
						t.Fatalf("OrderedMap.add[%d] = %v, want %v", i, got[i], v.ExpectedOmapAdds[i])
					}
				}
			}
		})
	}
}

// TestTurnstileBadOpcode pins the oracle-null behaviors: unknown opcode
// indices and skipped paths yield a typed error (never a panic), and
// undecodable payloads are classified turnstile_invalid.
func TestTurnstileBadOpcode(t *testing.T) {
	var sawUnknownOpcode bool
	for _, v := range loadTurnstileVectors(t) {
		if !v.OracleNull {
			continue
		}
		t.Run(v.Name, func(t *testing.T) {
			token, err := SolveTurnstile(v.DX, v.P)
			if err == nil {
				t.Fatalf("SolveTurnstile(%q) = %q, want a typed error (oracle returned None)", v.Name, token)
			}
			if token != "" {
				t.Fatalf("SolveTurnstile returned token %q alongside an error", token)
			}
			var se *SentinelError
			if !errors.As(err, &se) {
				t.Fatalf("error = %v, want *SentinelError", err)
			}
			if se.Code != v.GoError {
				t.Fatalf("error code = %q, want %q", se.Code, v.GoError)
			}
			if strings.Contains(v.Name, "unknown-opcode") {
				sawUnknownOpcode = true
			}
		})
	}
	if !sawUnknownOpcode {
		t.Fatal("fixture set lost the unknown-opcode vector")
	}
}
