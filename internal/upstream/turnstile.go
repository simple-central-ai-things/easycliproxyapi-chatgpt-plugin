package upstream

// turnstile.go ports the reference's utils/turnstile.py solve_turnstile_token
// — the ~184-line pure-stdlib bytecode VM that emulates a browser
// environment (process_map is the "page" state) and returns the token the
// program sets. The port is opcode-for-opcode: the same registered slots
// (the callable opcodes below plus the data nodes 9/10/16), the same
// emulated browser strings, and the same error-swallowing main loop
// (`except Exception: continue` → every internal failure skips one opcode).
//
// The token the oracle emits is base64 of the literal argument to op 3;
// the rest of the program is environment simulation that the fixtures gate
// on through op 20 equality checks (see testdata/turnstile/_note.txt).

import (
	"encoding/base64"
	"fmt"
	"math"
	"math/rand/v2"
	"strings"
	"time"
)

// Sentinel errors the VM reports. The oracle returns None for every
// failure; the port surfaces the failure cause instead (the reference would
// otherwise post an EMPTY turnstile token to finalize and let the server
// reject it — failing typed is the deliberate divergence, pinned by the
// fixture go_error field).
var (
	ErrTurnstileInvalid = &SentinelError{
		Code:      "turnstile_invalid",
		message:   "the turnstile dx payload does not decode to a program",
		Retryable: false,
	}
	ErrTurnstileUnsolvable = &SentinelError{
		Code:      "turnstile_unsolved",
		message:   "the turnstile VM finished without producing a token",
		Retryable: false,
	}
)

// SolveTurnstile runs the VM over dx (base64 of the p-xor-ed JSON program)
// and returns the token, byte-identical to solve_turnstile_token. p is the
// XOR key the handshake passes (the legacy requirements token itself); an
// empty p disables the XOR, like the oracle.
func SolveTurnstile(dx, p string) (string, error) {
	vm, err := solveTurnstileVM(dx, p)
	if err != nil {
		return "", err
	}
	return vm.result, nil
}

// solveTurnstileVM is the internal form that also exposes the finished VM
// (state, recorded OrderedMap.add calls) to the differential tests.
func solveTurnstileVM(dx, p string) (*turnstileVM, error) {
	decoded, err := base64.StdEncoding.DecodeString(dx)
	if err != nil {
		return nil, fmt.Errorf("turnstile dx is not base64: %w", ErrTurnstileInvalid)
	}
	text := string(decoded)
	if p != "" {
		text = xorString(text, p)
	}
	value, err := decodePyJSON([]byte(text))
	if err != nil {
		return nil, fmt.Errorf("turnstile dx does not decode to JSON: %w", ErrTurnstileInvalid)
	}
	program, ok := value.([]any)
	if !ok {
		// The oracle would iterate a non-list (its dict keys / string
		// characters) and never dispatch an opcode; treat it as empty.
		program = []any{}
	}
	vm := &turnstileVM{
		env: map[any]any{
			1: opID(opXorAssign), 2: opID(opAssign), 3: opID(opSetResult),
			5: opID(opConcat), 6: opID(opDottedLocation), 7: opID(opCall),
			8:  opID(opCopy),
			9:  program,
			10: "window",
			14: opID(opJSONParse), 15: opID(opJSONDumps),
			16: p,
			17: opID(opInvoke), 18: opID(opB64Decode), 19: opID(opB64Encode),
			20: opID(opConditionalCall), 21: opID(opNoop),
			23: opID(opRawCall), 24: opID(opDottedJoin),
		},
		startNS: time.Now().UnixNano(),
	}
	vm.rng = rand.New(rand.NewPCG(uint64(vm.startNS), 0))

	for _, tok := range program {
		entry, isEntry := tok.([]any)
		if !isEntry || len(entry) == 0 {
			continue // oracle: IndexError/TypeError on token[0] -> continue
		}
		key := pyNormalizeKey(entry[0])
		if !keyHashable(key) {
			continue // oracle: unhashable token[0] -> TypeError -> continue
		}
		fn, exists := vm.env[key]
		if !exists {
			continue // unknown opcode index: the oracle skips it silently
		}
		op, isOp := fn.(opID)
		if !isOp {
			continue // data nodes (9/10/16) are not callable
		}
		vm.runOp(op, entry[1:])
	}

	if vm.result == "" {
		return nil, fmt.Errorf("turnstile program produced no token: %w", ErrTurnstileUnsolvable)
	}
	return vm, nil
}

// turnstileVM is one solve: the emulated page plus the VM bookkeeping.
type turnstileVM struct {
	env     map[any]any
	result  string
	startNS int64
	rng     *rand.Rand
	// omapAdds records every OrderedMap.add call in program order — the
	// differential hook the driver's monkeypatch provides on the oracle
	// side (the oracle closure is not otherwise inspectable).
	omapAdds [][]any
}

// opID marks a builtin opcode registered in the environment.
type opID int

const (
	opXorAssign       opID = 1
	opAssign          opID = 2
	opSetResult       opID = 3
	opConcat          opID = 5
	opDottedLocation  opID = 6
	opCall            opID = 7
	opCopy            opID = 8
	opJSONParse       opID = 14
	opJSONDumps       opID = 15
	opInvoke          opID = 17
	opB64Decode       opID = 18
	opB64Encode       opID = 19
	opConditionalCall opID = 20
	opNoop            opID = 21
	opRawCall         opID = 23
	opDottedJoin      opID = 24
)

// orderedMap is the oracle's OrderedMap: insertion-ordered key/value store
// the bytecode fills through window.Reflect.set.
type orderedMap struct {
	keys   []string
	values map[string]any
}

func newOrderedMap() *orderedMap {
	return &orderedMap{values: make(map[string]any)}
}

func (m *orderedMap) add(key string, value any) {
	if _, ok := m.values[key]; !ok {
		m.keys = append(m.keys, key)
	}
	m.values[key] = value
}

// runOp dispatches one opcode with the oracle's except-continue semantics:
// any internal failure (including a Go panic on a path the oracle would
// raise on) skips the opcode and the program continues.
func (vm *turnstileVM) runOp(op opID, args []any) {
	defer func() {
		_ = recover()
	}()
	switch op {
	case opXorAssign:
		vm.opXorAssign(args)
	case opAssign:
		vm.opAssign(args)
	case opSetResult:
		vm.opSetResult(args)
	case opConcat:
		vm.opConcat(args)
	case opDottedLocation:
		vm.opDottedLocation(args)
	case opCall:
		vm.opCall(args)
	case opCopy:
		vm.opCopy(args)
	case opJSONParse:
		vm.opJSONParse(args)
	case opJSONDumps:
		vm.opJSONDumps(args)
	case opInvoke:
		vm.opInvoke(args)
	case opB64Decode:
		vm.opB64Decode(args)
	case opB64Encode:
		vm.opB64Encode(args)
	case opConditionalCall:
		vm.opConditionalCall(args)
	case opNoop:
		// the oracle's func_21 ignores every argument
	case opRawCall:
		vm.opRawCall(args)
	case opDottedJoin:
		vm.opDottedJoin(args)
	}
}

// get/set are the oracle's process_map[e] reads/writes: missing keys and
// unhashable keys both raise in Python and are swallowed by the main loop,
// so they are plain skips here.
func (vm *turnstileVM) get(key any) (any, bool) {
	key = pyNormalizeKey(key)
	if !keyHashable(key) {
		return nil, false
	}
	value, ok := vm.env[key]
	return value, ok
}

func (vm *turnstileVM) set(key, value any) {
	key = pyNormalizeKey(key)
	if !keyHashable(key) {
		return
	}
	vm.env[key] = value
}

// pyNormalizeKey folds integer-valued floats into ints so a JSON 1.0
// addresses the same slot as 1, like Python's hash(1.0) == hash(1).
func pyNormalizeKey(key any) any {
	if f, ok := key.(float64); ok && f == math.Trunc(f) && math.Abs(f) < 9e15 {
		return int(f)
	}
	return key
}

func keyHashable(key any) bool {
	// Non-comparable Go types panic on map access; the oracle raises
	// TypeError on them and the main loop swallows it.
	switch key.(type) {
	case []any, pyObj, map[string]any:
		return false
	}
	return true
}

// toStr is the oracle's _turnstile_to_str: None -> "undefined", floats via
// repr, strings through the emulated browser special map, all-string lists
// joined with ",", everything else via str().
func (vm *turnstileVM) toStr(value any) string {
	switch v := value.(type) {
	case nil:
		return "undefined"
	case float64:
		return pyFloatString(v)
	case string:
		if special, ok := turnstileSpecialStrings[v]; ok {
			return special
		}
		return v
	case []any:
		allStrings := true
		for _, item := range v {
			if _, ok := item.(string); !ok {
				allStrings = false
				break
			}
		}
		if allStrings {
			items := make([]string, len(v))
			for i, item := range v {
				items[i] = item.(string)
			}
			return strings.Join(items, ",")
		}
		return pythonStr(v)
	default:
		return pythonStr(v)
	}
}

// turnstileSpecialStrings is the oracle's emulated browser string table.
var turnstileSpecialStrings = map[string]string{
	"window.Math":            "[object Math]",
	"window.Reflect":         "[object Reflect]",
	"window.performance":     "[object Performance]",
	"window.localStorage":    "[object Storage]",
	"window.Object":          "function Object() { [native code] }",
	"window.Reflect.set":     "function set() { [native code] }",
	"window.performance.now": "function () { [native code] }",
	"window.Object.create":   "function create() { [native code] }",
	"window.Object.keys":     "function keys() { [native code] }",
	"window.Math.random":     "function random() { [native code] }",
}

// turnstileLocalStorageKeys is the oracle's pinned Object.keys(localStorage)
// result.
var turnstileLocalStorageKeys = []any{
	"STATSIG_LOCAL_STORAGE_INTERNAL_STORE_V4",
	"STATSIG_LOCAL_STORAGE_STABLE_ID",
	"client-correlated-secret",
	"oai/apps/capExpiresAt",
	"oai-did",
	"STATSIG_LOCAL_STORAGE_LOGGING_REQUEST",
	"UiState.isNavigationCollapsed.1",
}

// xorString is the oracle's _xor_string: rune-wise XOR with the key
// cycling, disabled (identity) for an empty key. Non-ASCII input XORs by
// code point, like Python's ord/chr.
func xorString(text, key string) string {
	if key == "" {
		return text
	}
	keyRunes := []rune(key)
	out := make([]rune, 0, len(text))
	for i, r := range []rune(text) {
		out = append(out, r^keyRunes[i%len(keyRunes)])
	}
	return string(out)
}

// pyStrValue is Python's str(x) where x is guaranteed printable-scalar:
// strings stay as-is, everything else renders like str().
func pyStrValue(value any) string {
	if s, ok := value.(string); ok {
		return s
	}
	return pythonStr(value)
}

func (vm *turnstileVM) opXorAssign(args []any) {
	if len(args) != 2 {
		return
	}
	current, ok1 := vm.get(args[0])
	source, ok2 := vm.get(args[1])
	if !ok1 || !ok2 {
		return
	}
	vm.set(args[0], xorString(vm.toStr(current), vm.toStr(source)))
}

func (vm *turnstileVM) opAssign(args []any) {
	if len(args) != 2 {
		return
	}
	vm.set(args[0], args[1]) // the value is a LITERAL, not a slot
}

func (vm *turnstileVM) opSetResult(args []any) {
	if len(args) != 1 {
		return
	}
	literal, ok := args[0].(string)
	if !ok {
		return // oracle: AttributeError on non-str -> swallowed
	}
	vm.result = base64.StdEncoding.EncodeToString([]byte(literal))
}

func (vm *turnstileVM) opConcat(args []any) {
	if len(args) != 2 {
		return
	}
	current, ok1 := vm.get(args[0])
	incoming, ok2 := vm.get(args[1])
	if !ok1 || !ok2 {
		return
	}
	if list, ok := current.([]any); ok {
		vm.set(args[0], append(append([]any{}, list...), incoming))
		return
	}
	if isPyStrOrFloat(current) || isPyStrOrFloat(incoming) {
		vm.set(args[0], vm.toStr(current)+vm.toStr(incoming))
		return
	}
	vm.set(args[0], "NaN")
}

func isPyStrOrFloat(value any) bool {
	switch value.(type) {
	case string, float64:
		return true
	}
	return false
}

func (vm *turnstileVM) opDottedLocation(args []any) {
	if len(args) != 3 {
		return
	}
	tv, ok1 := vm.get(args[1])
	nv, ok2 := vm.get(args[2])
	if !ok1 || !ok2 {
		return
	}
	top, isTop := tv.(string)
	sub, isSub := nv.(string)
	if !isTop || !isSub {
		return // oracle: nothing is stored unless both sides are strings
	}
	value := top + "." + sub
	if value == "window.document.location" {
		value = "https://chatgpt.com/"
	}
	vm.set(args[0], value)
}

func (vm *turnstileVM) opCall(args []any) {
	if len(args) < 1 {
		return
	}
	target, ok := vm.get(args[0])
	if !ok {
		return
	}
	values := make([]any, 0, len(args)-1)
	for _, arg := range args[1:] {
		value, ok := vm.get(arg)
		if !ok {
			return // oracle: KeyError mid-comprehension -> swallowed
		}
		values = append(values, value)
	}
	switch t := target.(type) {
	case string:
		if t != "window.Reflect.set" {
			return // plain strings are not callable in the oracle
		}
		if len(values) != 3 {
			return // oracle: unpack ValueError -> swallowed
		}
		omap, ok := values[0].(*orderedMap)
		if !ok {
			return // oracle: AttributeError -> swallowed
		}
		vm.omapAdds = append(vm.omapAdds, []any{values[1], values[2]})
		omap.add(pyStrValue(values[1]), values[2])
	case opID:
		vm.runOp(t, values)
	}
}

func (vm *turnstileVM) opCopy(args []any) {
	if len(args) != 2 {
		return
	}
	source, ok := vm.get(args[1])
	if !ok {
		return
	}
	vm.set(args[0], source)
}

func (vm *turnstileVM) opJSONParse(args []any) {
	if len(args) != 2 {
		return
	}
	raw, ok := vm.get(args[1])
	if !ok {
		return
	}
	text, isText := raw.(string)
	if !isText {
		return // oracle: TypeError -> swallowed
	}
	value, err := decodePyJSON([]byte(text))
	if err != nil {
		return
	}
	vm.set(args[0], value)
}

func (vm *turnstileVM) opJSONDumps(args []any) {
	if len(args) != 2 {
		return
	}
	value, ok := vm.get(args[1])
	if !ok {
		return
	}
	raw, err := appendPyJSONDefault(nil, value)
	if err != nil {
		return // oracle: TypeError (e.g. an OrderedMap) -> swallowed
	}
	vm.set(args[0], string(raw))
}

func (vm *turnstileVM) opInvoke(args []any) {
	if len(args) < 2 {
		return
	}
	target, ok := vm.get(args[1])
	if !ok {
		return
	}
	callArgs := make([]any, 0, len(args)-2)
	for _, arg := range args[2:] {
		value, ok := vm.get(arg)
		if !ok {
			return // oracle: KeyError mid-comprehension -> swallowed
		}
		callArgs = append(callArgs, value)
	}
	switch t := target.(type) {
	case string:
		switch t {
		case "window.performance.now":
			elapsed := time.Now().UnixNano() - vm.startNS
			vm.set(args[0], (float64(elapsed)+vm.rng.Float64())/1e6)
		case "window.Object.create":
			vm.set(args[0], newOrderedMap())
		case "window.Object.keys":
			if len(callArgs) > 0 && pyEquals(callArgs[0], "window.localStorage") {
				vm.set(args[0], turnstileLocalStorageKeys)
			}
			// a non-matching first argument stores nothing, like the oracle
		case "window.Math.random":
			vm.set(args[0], vm.rng.Float64())
		}
	case opID:
		vm.runOp(t, callArgs)
		vm.set(args[0], nil) // the oracle stores the callable's None return
	}
}

func (vm *turnstileVM) opB64Decode(args []any) {
	if len(args) != 1 {
		return
	}
	value, ok := vm.get(args[0])
	if !ok {
		return
	}
	raw, err := base64.StdEncoding.DecodeString(vm.toStr(value))
	if err != nil {
		return // oracle: binascii.Error -> swallowed
	}
	vm.set(args[0], string(raw))
}

func (vm *turnstileVM) opB64Encode(args []any) {
	if len(args) != 1 {
		return
	}
	value, ok := vm.get(args[0])
	if !ok {
		return
	}
	vm.set(args[0], base64.StdEncoding.EncodeToString([]byte(vm.toStr(value))))
}

func (vm *turnstileVM) opConditionalCall(args []any) {
	if len(args) < 3 {
		return
	}
	left, ok1 := vm.get(args[0])
	right, ok2 := vm.get(args[1])
	if !ok1 || !ok2 {
		return
	}
	if !pyEquals(left, right) {
		return
	}
	target, ok := vm.get(args[2])
	if !ok {
		return
	}
	values := make([]any, 0, len(args)-3)
	for _, arg := range args[3:] {
		value, ok := vm.get(arg)
		if !ok {
			return // oracle: KeyError mid-comprehension -> swallowed
		}
		values = append(values, value)
	}
	if op, isOp := target.(opID); isOp {
		vm.runOp(op, values)
	}
}

func (vm *turnstileVM) opRawCall(args []any) {
	if len(args) < 2 {
		return
	}
	guard, ok := vm.get(args[0])
	if !ok {
		return
	}
	if guard == nil {
		return // oracle: `is not None`
	}
	target, ok := vm.get(args[1])
	if !ok {
		return
	}
	if op, isOp := target.(opID); isOp {
		vm.runOp(op, args[2:]) // RAW literals — the oracle does not resolve them
	}
}

func (vm *turnstileVM) opDottedJoin(args []any) {
	if len(args) != 3 {
		return
	}
	tv, ok1 := vm.get(args[1])
	nv, ok2 := vm.get(args[2])
	if !ok1 || !ok2 {
		return
	}
	top, isTop := tv.(string)
	sub, isSub := nv.(string)
	if !isTop || !isSub {
		return
	}
	vm.set(args[0], top+"."+sub)
}
