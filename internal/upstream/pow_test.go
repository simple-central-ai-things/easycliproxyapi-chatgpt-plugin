package upstream

import (
	"bytes"
	"crypto/sha3"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Fixture loaders below decode vectors.json with decodePyJSON so integer
// literals stay ints (Python json.loads semantics) — the byte-for-byte
// comparison against the oracle requires the exact int/float split.

type legacyVector struct {
	Name          string
	UserAgent     string
	ScriptSources []string
	DataBuild     string
	Config        []any
	ExpectedToken string
}

func loadLegacyVectors(t *testing.T) []legacyVector {
	t.Helper()
	var out []legacyVector
	for _, obj := range fixtureObjects(t, "legacy-token") {
		v := legacyVector{
			Name:          fixtureString(t, obj, "name"),
			UserAgent:     fixtureString(t, obj, "user_agent"),
			ScriptSources: fixtureStrings(t, obj, "script_sources"),
			DataBuild:     fixtureString(t, obj, "data_build"),
			Config:        fixtureArray(t, obj, "config"),
			ExpectedToken: fixtureString(t, obj, "expected_token"),
		}
		out = append(out, v)
	}
	return out
}

type proofVector struct {
	Name             string
	Seed             string
	Difficulty       string
	Config           []any
	MaxTries         int
	ExpectedToken    string
	ExpectedAttempts int
}

func loadProofVectors(t *testing.T) []proofVector {
	t.Helper()
	var out []proofVector
	for _, obj := range fixtureObjects(t, "proof") {
		out = append(out, proofVector{
			Name:             fixtureString(t, obj, "name"),
			Seed:             fixtureString(t, obj, "seed"),
			Difficulty:       fixtureString(t, obj, "difficulty"),
			Config:           fixtureArray(t, obj, "config"),
			MaxTries:         fixtureInt(t, obj, "max_tries"),
			ExpectedToken:    fixtureString(t, obj, "expected_token"),
			ExpectedAttempts: fixtureInt(t, obj, "expected_attempts"),
		})
	}
	return out
}

func fixtureObjects(t *testing.T, dir string) []pyObj {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", dir, "vectors.json"))
	if err != nil {
		t.Fatalf("read fixture %s: %v", dir, err)
	}
	value, err := decodePyJSON(raw)
	if err != nil {
		t.Fatalf("parse fixture %s: %v", dir, err)
	}
	arr, ok := value.([]any)
	if !ok || len(arr) == 0 {
		t.Fatalf("fixture %s must be a non-empty array", dir)
	}
	out := make([]pyObj, 0, len(arr))
	for _, item := range arr {
		obj, ok := item.(pyObj)
		if !ok {
			t.Fatalf("fixture %s entry is not an object", dir)
		}
		out = append(out, obj)
	}
	return out
}

func fixtureField(t *testing.T, obj pyObj, key string) any {
	t.Helper()
	v, ok := fixtureFieldOpt(obj, key)
	if !ok {
		t.Fatalf("fixture object missing field %q", key)
	}
	return v
}

// fixtureFieldOpt is the optional-field variant: absent keys are legal in
// fixture objects (only present fields are asserted).
func fixtureFieldOpt(obj pyObj, key string) (any, bool) {
	for _, pair := range obj.pairs {
		if pair.key == key {
			return pair.val, true
		}
	}
	return nil, false
}

func fixtureString(t *testing.T, obj pyObj, key string) string {
	t.Helper()
	v, ok := fixtureField(t, obj, key).(string)
	if !ok {
		t.Fatalf("fixture field %q is not a string", key)
	}
	return v
}

func fixtureInt(t *testing.T, obj pyObj, key string) int {
	t.Helper()
	v, ok := fixtureField(t, obj, key).(int)
	if !ok {
		t.Fatalf("fixture field %q is not an integer", key)
	}
	return v
}

func fixtureArray(t *testing.T, obj pyObj, key string) []any {
	t.Helper()
	v, ok := fixtureField(t, obj, key).([]any)
	if !ok {
		t.Fatalf("fixture field %q is not an array", key)
	}
	return v
}

func fixtureStrings(t *testing.T, obj pyObj, key string) []string {
	t.Helper()
	var out []string
	for _, v := range fixtureArray(t, obj, key) {
		s, ok := v.(string)
		if !ok {
			t.Fatalf("fixture field %q contains a non-string", key)
		}
		out = append(out, s)
	}
	return out
}

// TestLegacyTokenShape pins the 25-field config builder (field order and
// literal values) and the "gAAAAAC"+base64 token, differentially: every
// fixture config must serialize byte-identically to the oracle's
// json.dumps(separators=(",", ":"), ensure_ascii=False).
func TestLegacyTokenShape(t *testing.T) {
	t.Run("differential", func(t *testing.T) {
		for _, v := range loadLegacyVectors(t) {
			t.Run(v.Name, func(t *testing.T) {
				got, err := BuildLegacyRequirementsToken(v.Config)
				if err != nil {
					t.Fatalf("BuildLegacyRequirementsToken: %v", err)
				}
				if got != v.ExpectedToken {
					t.Fatalf("legacy token mismatch:\n got: %s\nwant: %s", got, v.ExpectedToken)
				}
			})
		}
	})

	t.Run("config shape", func(t *testing.T) {
		ua := "Mozilla/5.0 (fixture)"
		sources := []string{"https://chatgpt.com/backend-api/c/abc/_x/app.js", "https://chatgpt.com/assets/y.js"}
		build := "c/abc/"
		cfg := BuildPowConfig(ua, sources, build)

		if len(cfg) != 25 {
			t.Fatalf("BuildPowConfig returned %d fields, want 25 (oracle field order)", len(cfg))
		}
		resolutions := map[int]bool{}
		for _, r := range powScreenResolutions {
			resolutions[r[0]+r[1]] = true
		}
		keyUniverse := map[string]bool{}
		for _, keys := range [][]string{powNavigatorKeys, powDocumentKeys, powWindowKeys} {
			for _, k := range keys {
				keyUniverse[k] = true
			}
		}
		sourceUniverse := map[string]bool{powDefaultScript: true}
		for _, s := range sources {
			sourceUniverse[s] = true
		}
		timeRe := regexp.MustCompile(`^[A-Z][a-z]{2} [A-Z][a-z]{2} \d{2} \d{4} \d{2}:\d{2}:\d{2} GMT-0500 \(Eastern Standard Time\)$`)

		checks := []struct {
			index int
			check func(any) bool
			desc  string
		}{
			{0, func(v any) bool { n, ok := v.(int); return ok && resolutions[n] }, "resolution sum"},
			{1, func(v any) bool { s, ok := v.(string); return ok && timeRe.MatchString(s) }, "legacy parse time"},
			{2, func(v any) bool { return v == 4294705152 }, "literal 4294705152"},
			{3, func(v any) bool { return v == 1 }, "literal 1"},
			{4, func(v any) bool { return v == ua }, "user agent"},
			{5, func(v any) bool { s, ok := v.(string); return ok && sourceUniverse[s] }, "script source choice"},
			{6, func(v any) bool { return v == build }, "data build"},
			{7, func(v any) bool { return v == "en-US" }, "literal en-US"},
			{8, func(v any) bool { return v == "en-US,es-US,en,es" }, "literal accept languages"},
			{9, func(v any) bool { f, ok := v.(float64); return ok && f >= 0 && f < 1 }, "random()"},
			{10, func(v any) bool { s, ok := v.(string); return ok && keyUniverse[s] }, "navigator key choice"},
			{11, func(v any) bool { s, ok := v.(string); return ok && keyUniverse[s] }, "document key choice"},
			{12, func(v any) bool { s, ok := v.(string); return ok && keyUniverse[s] }, "window key choice"},
			{13, func(v any) bool { f, ok := v.(float64); return ok && f > 0 }, "perf_counter ms"},
			{14, func(v any) bool { s, ok := v.(string); return ok && isUUIDv4(s) }, "new uuid"},
			{15, func(v any) bool { return v == "" }, "empty string"},
			{16, func(v any) bool { n, ok := v.(int); return ok && (n == 8 || n == 16 || n == 24 || n == 32) }, "core choice"},
			{17, func(v any) bool { f, ok := v.(float64); return ok && f > 0 }, "clock offset"},
		}
		for _, c := range checks {
			if !c.check(cfg[c.index]) {
				t.Fatalf("config[%d] = %#v, want %s", c.index, cfg[c.index], c.desc)
			}
		}
		for i := 18; i <= 24; i++ {
			if cfg[i] != 0 {
				t.Fatalf("config[%d] = %#v, want literal 0", i, cfg[i])
			}
		}

		// The token built from a live config must decode back to 25 fields
		// with the gAAAAAC prefix (the server-facing shape contract).
		token, err := BuildLegacyRequirementsToken(cfg)
		if err != nil {
			t.Fatalf("BuildLegacyRequirementsToken(live cfg): %v", err)
		}
		if !strings.HasPrefix(token, "gAAAAAC") {
			t.Fatalf("legacy token = %q, want gAAAAAC prefix", token[:8])
		}
		payload, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(token, "gAAAAAC"))
		if err != nil {
			t.Fatalf("legacy token payload is not base64: %v", err)
		}
		arr, err := decodePyJSON(payload)
		if err != nil {
			t.Fatalf("legacy token payload is not JSON: %v", err)
		}
		if len(arr.([]any)) != 25 {
			t.Fatalf("legacy token payload has %d fields, want 25", len(arr.([]any)))
		}
	})
}

// TestMineProofFindsNonce verifies the differential proof vectors: the Go
// miner must return the oracle's exact token (byte-identical), and the mined
// nonce must be the FIRST acceptable in sequential order — every earlier
// attempt fails the predicate, the mined one passes it.
func TestMineProofFindsNonce(t *testing.T) {
	for _, v := range loadProofVectors(t) {
		t.Run(v.Name, func(t *testing.T) {
			token, ok := MineProof(v.Seed, v.Difficulty, v.Config, v.MaxTries)
			if v.ExpectedToken == "" {
				// Refusal vector: the oracle raises / cannot solve within
				// the limit; the Go contract is ("", false).
				if ok || token != "" {
					t.Fatalf("MineProof(difficulty=%q) = (%q, %v), want (\"\", false)", v.Difficulty, token, ok)
				}
				return
			}
			if !ok {
				t.Fatalf("MineProof(difficulty=%q): not solved within %d tries", v.Difficulty, v.MaxTries)
			}
			if token != v.ExpectedToken {
				t.Fatalf("proof token mismatch:\n got: %s\nwant: %s", token, v.ExpectedToken)
			}
			// Re-verify iteration order: decode the mined proof, recover the
			// nonce at field 3 (field 4 is nonce>>1), then check the oracle
			// predicate for every attempt up to it.
			payload, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(token, "gAAAAAB"))
			if err != nil {
				t.Fatalf("proof token payload is not base64: %v", err)
			}
			arr, err := decodePyJSON(payload)
			if err != nil {
				t.Fatalf("proof token payload is not JSON: %v", err)
			}
			fields, isArr := arr.([]any)
			if !isArr || len(fields) != 25 {
				t.Fatalf("proof payload is %T (%d fields), want a 25-field array", arr, len(fields))
			}
			nonce, isInt := fields[3].(int)
			if !isInt {
				t.Fatalf("proof payload[3] = %#v, want the mined nonce (int)", fields[3])
			}
			// The proof JSON is [c0,c1,c2, nonce, c4,c5,c6,c7,c8, nonce>>1, c10...c24]:
			// the nonce lands at index 3 and nonce>>1 at index 9.
			if fields[9] != nonce>>1 {
				t.Fatalf("proof payload[9] = %#v, want nonce>>1 (%d)", fields[9], nonce>>1)
			}
			if nonce+1 != v.ExpectedAttempts {
				t.Fatalf("mined nonce %d implies attempt %d, fixture says %d", nonce, nonce+1, v.ExpectedAttempts)
			}
			static1, static2, static3, err := proofStaticPieces(v.Config)
			if err != nil {
				t.Fatalf("proofStaticPieces: %v", err)
			}
			for j := 0; j <= nonce; j++ {
				accepted := proofAccepts(v.Seed, v.Difficulty, static1, static2, static3, j)
				if j < nonce && accepted {
					t.Fatalf("attempt %d (before the mined %d) already accepts — iteration order diverges from the oracle", j, nonce)
				}
				if j == nonce && !accepted {
					t.Fatalf("mined attempt %d does not satisfy the predicate", nonce)
				}
			}
		})
	}
}

// proofAccepts is the predicate MineProof applies to one attempt (the test
// re-runs it to verify first-acceptable order; it must stay in lockstep
// with proofStaticPieces + the sha3-512 comparison in MineProof).
func proofAccepts(seed, difficulty string, static1, static2, static3 []byte, nonce int) bool {
	finalJSON := append(append(append(append(append([]byte{}, static1...), strconv.Itoa(nonce)...), static2...), strconv.Itoa(nonce>>1)...), static3...)
	encoded := base64.StdEncoding.EncodeToString(finalJSON)
	target := mustHexDecode(difficulty)
	diffLen := len(difficulty) / 2
	sum := sha3.Sum512(append([]byte(seed), encoded...))
	return bytes.Compare(sum[:diffLen], target) <= 0
}

func mustHexDecode(s string) []byte {
	out, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return out
}

// TestMineProofRefusesEasyDifficultyZero pins the refusal semantics: a
// zero-valued difficulty string ("0" — odd-length, so the oracle's
// bytes.fromhex raises) is refused with ok=false instead of solved.
func TestMineProofRefusesEasyDifficultyZero(t *testing.T) {
	vectors := loadLegacyVectors(t)
	cfg := vectors[0].Config

	for _, difficulty := range []string{"0", "zz"} {
		t.Run("difficulty="+strconv.Quote(difficulty), func(t *testing.T) {
			token, ok := MineProof("fixture-seed-reject", difficulty, cfg, 500000)
			if ok || token != "" {
				t.Fatalf("MineProof(difficulty=%q) = (%q, %v), want (\"\", false)", difficulty, token, ok)
			}
		})
	}

	// Contrast: a real 1-byte upper bound still solves quickly.
	if _, ok := MineProof("fixture-seed-001", "ab", cfg, 500000); !ok {
		t.Fatal("difficulty \"ab\" must solve within the limit")
	}
}
