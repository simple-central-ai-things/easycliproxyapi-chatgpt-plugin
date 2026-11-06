package upstream

// pow.go ports the reference's utils/pow.py: the 25-field browser
// fingerprint config, the legacy requirements token ("gAAAAAC" + base64 of
// the config in the oracle's exact JSON dialect) and the SHA3-512
// proof-of-work miner ("gAAAAAB" + base64 of the config with the nonce
// spliced in). The literal choice universes live in pow_constants.go,
// extracted byte-for-byte from the oracle.

import (
	"bytes"
	"crypto/sha3"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math/rand/v2"
	"strconv"
	"time"
)

// processStart anchors the perf-counter-style uptime floats (config fields
// 13 and 17) the way the oracle's time.perf_counter does: monotonic since
// start, arbitrary origin.
var processStart = time.Now()

// sentinelPowMaxTries is the oracle's _pow_generate attempt limit.
const sentinelPowMaxTries = 500000

// BuildPowConfig returns the 25-field browser-fingerprint array exactly as
// the oracle's build_pow_config assembles it: same field order, same literal
// pins (4294705152 at 2, 1 at 3, the language pair at 7/8, "" at 15, zeros
// at 18-24) and the same choice universes; the random and time-derived
// fields are generated live.
func BuildPowConfig(userAgent string, scriptSources []string, dataBuild string) []any {
	scriptSource := powDefaultScript
	if len(scriptSources) > 0 {
		scriptSource = scriptSources[rand.IntN(len(scriptSources))]
	}
	resolution := powScreenResolutions[rand.IntN(len(powScreenResolutions))]
	uptimeMs := float64(time.Since(processStart).Nanoseconds()) / 1e6
	return []any{
		resolution[0] + resolution[1],
		legacyParseTime(),
		4294705152,
		1,
		userAgent,
		scriptSource,
		dataBuild,
		"en-US",
		"en-US,es-US,en,es",
		rand.Float64(),
		powNavigatorKeys[rand.IntN(len(powNavigatorKeys))],
		powDocumentKeys[rand.IntN(len(powDocumentKeys))],
		powWindowKeys[rand.IntN(len(powWindowKeys))],
		uptimeMs,
		newUUID(),
		"",
		powCores[rand.IntN(len(powCores))],
		float64(time.Now().UnixMilli()) - uptimeMs,
		0, 0, 0, 0, 0, 0, 0,
	}
}

// legacyParseTime renders the current time in the oracle's pinned form: the
// UTC-5 wall clock (the reference's fixed -5h offset, not real DST-aware
// Eastern time) plus the literal "GMT-0500 (Eastern Standard Time)" suffix.
func legacyParseTime() string {
	now := time.Now().UTC().Add(-5 * time.Hour)
	return now.Format("Mon Jan 02 2006 15:04:05") + " GMT-0500 (Eastern Standard Time)"
}

// BuildLegacyRequirementsToken renders config in the oracle's exact token
// form: "gAAAAAC" + base64(json.dumps(config, separators=(",", ":"),
// ensure_ascii=False)). The serialization is byte-for-byte the oracle's —
// see pyjson.go for the dialect rules the fixtures pin.
func BuildLegacyRequirementsToken(config []any) (string, error) {
	raw, err := appendPyJSONCompact(nil, config)
	if err != nil {
		return "", fmt.Errorf("build legacy requirements token: %w", err)
	}
	return "gAAAAAC" + base64.StdEncoding.EncodeToString(raw), nil
}

// MineProof searches nonces i = 0, 1, 2, ... in the oracle's sequential
// order for the first i whose proof JSON hashes below the difficulty bound,
// and returns "gAAAAAB" + base64(proofJSON). The proof JSON is the 25-field
// config with index 3 replaced by the nonce i and index 9 by i>>1 (config
// entries 3 and 9 are otherwise dropped from the serialization — see
// proofStaticPieces); the predicate is
// sha3_512(seed + base64(proofJSON))[:len(difficulty)/2] <= bytes.fromhex(difficulty).
//
// ok is false when no nonce accepts within maxTries OR when the difficulty
// is not even-length hex — the oracle raises ValueError there (and
// build_proof_token raises on unsolved), so both map to refusal. The
// oracle's unused "wQ8Lk5FbGpA2NcR9dShT6gYjU7VxZ4D" fallback string is not
// part of this contract: build_proof_token discards it.
func MineProof(seed, difficulty string, config []any, maxTries int) (string, bool) {
	target, err := hex.DecodeString(difficulty)
	if err != nil || len(difficulty)%2 != 0 {
		return "", false
	}
	static1, static2, static3, err := proofStaticPieces(config)
	if err != nil {
		return "", false
	}
	seedBytes := []byte(seed)
	for i := 0; i < maxTries; i++ {
		finalJSON := append(append(append(append(append([]byte{}, static1...), strconv.Itoa(i)...), static2...), strconv.Itoa(i>>1)...), static3...)
		encoded := base64.StdEncoding.EncodeToString(finalJSON)
		sum := sha3.Sum512(append(seedBytes, encoded...))
		if bytes.Compare(sum[:len(difficulty)/2], target) <= 0 {
			return "gAAAAAB" + encoded, true
		}
	}
	return "", false
}

// proofStaticPieces builds the three config slices the oracle serializes
// ONCE outside the mining loop (_pow_generate): dumps(config[:3]) with its
// closing bracket replaced by a comma, dumps(config[4:9]) with both brackets
// stripped and wrapped in commas, and dumps(config[10:]) with its opening
// bracket stripped and prefixed by a comma. The loop then splices
// "i" and "i>>1" between the pieces. Slicing is lenient like Python's (a
// short config yields empty pieces, not a panic).
func proofStaticPieces(config []any) ([]byte, []byte, []byte, error) {
	head, err := appendPyJSONCompact(nil, pySlice(config, 0, 3))
	if err != nil {
		return nil, nil, nil, err
	}
	mid, err := appendPyJSONCompact(nil, pySlice(config, 4, 9))
	if err != nil {
		return nil, nil, nil, err
	}
	tail, err := appendPyJSONCompact(nil, pySlice(config, 10, len(config)))
	if err != nil {
		return nil, nil, nil, err
	}
	static1 := make([]byte, 0, len(head)+1)
	static1 = append(static1, head[:len(head)-1]...) // drop the closing "]"
	static1 = append(static1, ',')
	static2 := make([]byte, 0, len(mid)+2)
	static2 = append(static2, ',')
	static2 = append(static2, mid[1:len(mid)-1]...) // strip both brackets
	static2 = append(static2, ',')
	static3 := make([]byte, 0, len(tail)+1)
	static3 = append(static3, ',')
	static3 = append(static3, tail[1:]...) // strip the opening "["
	return static1, static2, static3, nil
}
