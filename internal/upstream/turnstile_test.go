package upstream

import (
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
		out = append(out, v)
	}
	return out
}

// TestTurnstileOpcodes runs every positive fixture vector through the Go VM
// and requires the byte-identical token. Each fixture program chains op-20
// equality gates (see testdata/turnstile/_note.txt), so the token is only
// reachable when each exercised opcode produced the oracle's exact state.
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
