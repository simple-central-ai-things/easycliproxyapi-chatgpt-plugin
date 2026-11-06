package upstream

// pyjson.go ports the two JSON dialects the oracle speaks, because the
// sentinel wire contract is byte-identity with the reference's
// json.dumps/json.loads behavior, not just semantic equivalence:
//
//   - decodePyJSON: json.loads semantics — integer literals stay ints,
//     fractional/exponent literals become float64, objects keep insertion
//     order (pyObj). Python's == and dict semantics that the turnstile VM
//     and the proof serializer rely on are defined over that split.
//
//   - appendPyJSONCompact / appendPyJSONDefault: json.dumps with the oracle's
//     two separator styles. Compact is (",", ":") + ensure_ascii=False
//     (the legacy requirements and proof tokens); default is (", ", ": ") +
//     ensure_ascii=True (the VM's window.Object... op 15 dumps branch).
//
//   - pyFloatString: Python float repr — the digits are shortest round-trip
//     (both languages agree), but the FORMAT boundary differs: Python uses
//     scientific notation below 1e-4 and at 1e16+, Go's encoder below 1e-6
//     and at 1e21+. The fixtures pin the window between them.
//
// Fixture-unreachable repr edge divergences (the golden fixtures never hit
// them, so the port keeps its simpler rule):
//
//   - pyReprEscape emits \0 for a NUL byte, where Python's repr emits \x00.
//   - pyReprString's quote choice ignores escape-minimization when the
//     string has both a backslash and the preferred quote; Python repr
//     would switch quotes to avoid the extra escape.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// pyObj is one insertion-ordered JSON object (a Python dict).
type pyObj struct{ pairs []pyPair }

type pyPair struct {
	key string
	val any
}

// find returns the stored value for key, in the oracle's dict lookup order.
func (o pyObj) find(key string) (any, bool) {
	for _, pair := range o.pairs {
		if pair.key == key {
			return pair.val, true
		}
	}
	return nil, false
}

// decodePyJSON parses one JSON document with the oracle's json.loads value
// split (ints stay ints, objects keep order). Trailing data is refused, like
// a strict json.loads.
func decodePyJSON(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	value, err := decodePyValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing data after JSON value")
	}
	return value, nil
}

func decodePyValue(dec *json.Decoder) (any, error) {
	token, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch tok := token.(type) {
	case json.Delim:
		switch tok {
		case '[':
			arr := []any{}
			for dec.More() {
				value, err := decodePyValue(dec)
				if err != nil {
					return nil, err
				}
				arr = append(arr, value)
			}
			if _, err := dec.Token(); err != nil { // consume ']'
				return nil, err
			}
			return arr, nil
		case '{':
			obj := pyObj{}
			for dec.More() {
				keyToken, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, isString := keyToken.(string)
				if !isString {
					return nil, fmt.Errorf("object key %v is not a string", keyToken)
				}
				value, err := decodePyValue(dec)
				if err != nil {
					return nil, err
				}
				// Python keeps the LAST duplicate key's value at the first
				// occurrence's position; replace in place.
				replaced := false
				for i := range obj.pairs {
					if obj.pairs[i].key == key {
						obj.pairs[i].val = value
						replaced = true
						break
					}
				}
				if !replaced {
					obj.pairs = append(obj.pairs, pyPair{key: key, val: value})
				}
			}
			if _, err := dec.Token(); err != nil { // consume '}'
				return nil, err
			}
			return obj, nil
		default:
			return nil, fmt.Errorf("unexpected closing delimiter %q in JSON value", tok)
		}
	case json.Number:
		return pyNumber(tok), nil
	case string, bool:
		return tok, nil
	case nil:
		return nil, nil
	default:
		return nil, fmt.Errorf("unexpected JSON token %v", tok)
	}
}

// pyNumber applies the oracle's int/float split to one literal: no '.', 'e'
// or 'E' means int (Python's arbitrary precision degrades to int64 here;
// beyond that the literal is kept verbatim, which the emitters write back
// exactly as Python would).
func pyNumber(n json.Number) any {
	s := n.String()
	if !strings.ContainsAny(s, ".eE") {
		if i, err := strconv.Atoi(s); err == nil {
			return i
		}
		if i, err := strconv.ParseInt(s, 10, 64); err == nil {
			return int(i)
		}
		return n
	}
	f, err := n.Float64()
	if err != nil {
		return n
	}
	return f
}

// pySlice mimics Python's config[a:b] slicing: out-of-range bounds clamp
// instead of panicking, and an empty result is nil (which serializes as []).
func pySlice(config []any, a, b int) []any {
	if a < 0 {
		a = 0
	}
	if b < a {
		b = a
	}
	if b > len(config) {
		b = len(config)
	}
	if a >= len(config) {
		return nil
	}
	return config[a:b]
}

// pyFloatString renders a float64 exactly as Python's str/repr does:
// shortest round-trip digits, scientific notation below 1e-4 and at 1e16+,
// and an integer-valued float always keeps its trailing ".0".
func pyFloatString(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	}
	scientific := strconv.FormatFloat(f, 'e', -1, 64)
	exp, err := strconv.Atoi(scientific[strings.IndexByte(scientific, 'e')+1:])
	if err != nil {
		return scientific // unreachable: FormatFloat always emits an exponent
	}
	if exp < -4 || exp >= 16 {
		return scientific
	}
	fixed := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.Contains(fixed, ".") {
		fixed += ".0"
	}
	return fixed
}

// appendPyJSONCompact is json.dumps(v, separators=(",", ":"),
// ensure_ascii=False) — the legacy requirements and proof token dialect.
func appendPyJSONCompact(dst []byte, value any) ([]byte, error) {
	return appendPyJSON(dst, value, false, ",", ":")
}

// appendPyJSONDefault is json.dumps(v) with Python's default separators
// (", ", ": ") and ensure_ascii=True — the turnstile VM's op 15 dialect.
func appendPyJSONDefault(dst []byte, value any) ([]byte, error) {
	return appendPyJSON(dst, value, true, ", ", ": ")
}

func appendPyJSON(dst []byte, value any, ascii bool, itemSep, kvSep string) ([]byte, error) {
	return appendPyJSONDepth(dst, value, ascii, itemSep, kvSep, 0)
}

// maxPyJSONDepth mirrors Python's circular-reference guard: json.dumps
// raises ValueError beyond ~1000 nesting levels; the VM swallows it. The
// port fails at a bounded depth instead of overflowing the stack.
const maxPyJSONDepth = 256

func appendPyJSONDepth(dst []byte, value any, ascii bool, itemSep, kvSep string, depth int) ([]byte, error) {
	if depth > maxPyJSONDepth {
		return nil, fmt.Errorf("json value nested beyond %d levels (circular reference?)", maxPyJSONDepth)
	}
	switch v := value.(type) {
	case nil:
		return append(dst, "null"...), nil
	case bool:
		if v {
			return append(dst, "true"...), nil
		}
		return append(dst, "false"...), nil
	case int:
		return strconv.AppendInt(dst, int64(v), 10), nil
	case int64:
		return strconv.AppendInt(dst, v, 10), nil
	case float64:
		return append(dst, pyFloatString(v)...), nil
	case json.Number:
		return append(dst, v.String()...), nil
	case string:
		if ascii {
			return pyJSONEscapeASCII(append(dst, '"'), v), nil
		}
		return pyJSONEscape(append(dst, '"'), v), nil
	case []any:
		dst = append(dst, '[')
		for i, item := range v {
			if i > 0 {
				dst = append(dst, itemSep...)
			}
			var err error
			dst, err = appendPyJSONDepth(dst, item, ascii, itemSep, kvSep, depth+1)
			if err != nil {
				return nil, err
			}
		}
		return append(dst, ']'), nil
	case pyObj:
		dst = append(dst, '{')
		for i, pair := range v.pairs {
			if i > 0 {
				dst = append(dst, itemSep...)
			}
			if ascii {
				dst = pyJSONEscapeASCII(append(dst, '"'), pair.key)
			} else {
				dst = pyJSONEscape(append(dst, '"'), pair.key)
			}
			dst = append(dst, kvSep...)
			var err error
			dst, err = appendPyJSONDepth(dst, pair.val, ascii, itemSep, kvSep, depth+1)
			if err != nil {
				return nil, err
			}
		}
		return append(dst, '}'), nil
	case *orderedMap:
		// The turnstile VM's OrderedMap (window.Object.create) serializes
		// as its insertion-ordered contents — the browser VM's
		// JSON.stringify(map) the challenges rely on. Deliberate divergence
		// from the oracle, whose OrderedMap class makes json.dumps raise
		// TypeError (the reference can therefore never dump a map; real
		// challenges dump the fingerprint map — see testdata/turnstile).
		dst = append(dst, '{')
		for i, key := range v.keys {
			if i > 0 {
				dst = append(dst, itemSep...)
			}
			if ascii {
				dst = pyJSONEscapeASCII(append(dst, '"'), key)
			} else {
				dst = pyJSONEscape(append(dst, '"'), key)
			}
			dst = append(dst, kvSep...)
			var err error
			dst, err = appendPyJSONDepth(dst, v.values[key], ascii, itemSep, kvSep, depth+1)
			if err != nil {
				return nil, err
			}
		}
		return append(dst, '}'), nil
	default:
		// The oracle's json.dumps raises TypeError here; callers treat the
		// error as a swallowed op failure.
		return nil, fmt.Errorf("value of type %T is not JSON-serializable like Python's", value)
	}
}

// pyJSONEscape renders a string the way ensure_ascii=False does: only quote,
// backslash and control characters escape; everything else (including
// non-ASCII) passes through as UTF-8.
func pyJSONEscape(dst []byte, s string) []byte {
	for _, r := range s {
		dst = pyJSONEscapeChar(dst, r, false)
	}
	return append(dst, '"')
}

// pyJSONEscapeASCII renders a string the way ensure_ascii=True does:
// everything above U+007F escapes as \uXXXX (astral planes as surrogate
// pairs).
func pyJSONEscapeASCII(dst []byte, s string) []byte {
	for _, r := range s {
		dst = pyJSONEscapeChar(dst, r, true)
	}
	return append(dst, '"')
}

func pyJSONEscapeChar(dst []byte, r rune, ascii bool) []byte {
	switch r {
	case '"':
		return append(dst, '\\', '"')
	case '\\':
		return append(dst, '\\', '\\')
	case '\n':
		return append(dst, '\\', 'n')
	case '\r':
		return append(dst, '\\', 'r')
	case '\t':
		return append(dst, '\\', 't')
	case '\b':
		return append(dst, '\\', 'b')
	case '\f':
		return append(dst, '\\', 'f')
	}
	if r < 0x20 {
		return fmt.Appendf(dst, `\u%04x`, r)
	}
	if ascii && r > 0x7e {
		if r <= math.MaxUint16 {
			return fmt.Appendf(dst, `\u%04x`, r)
		}
		r -= 0x10000
		return fmt.Appendf(dst, `\u%04x\u%04x`, 0xD800+r>>10, 0xDC00+r&0x3FF)
	}
	var tmp [4]byte
	n := utf8.EncodeRune(tmp[:], r)
	return append(dst, tmp[:n]...)
}

// pythonStr is Python's str() over the value set the port produces — the
// to_str fallback in the turnstile VM. Containers use repr() of their items
// (single quotes), exactly like str(["a", 1]) == "['a', 1]".
func pythonStr(value any) string {
	switch v := value.(type) {
	case nil:
		return "None"
	case bool:
		if v {
			return "True"
		}
		return "False"
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case float64:
		return pyFloatString(v)
	case json.Number:
		return v.String()
	case string:
		return v
	case []any:
		items := make([]string, len(v))
		for i, item := range v {
			items[i] = pyRepr(item)
		}
		return "[" + strings.Join(items, ", ") + "]"
	case pyObj:
		items := make([]string, len(v.pairs))
		for i, pair := range v.pairs {
			items[i] = pyReprString(pair.key) + ": " + pyRepr(pair.val)
		}
		return "{" + strings.Join(items, ", ") + "}"
	case *orderedMap:
		// The oracle renders this as "<utils.turnstile.OrderedMap object
		// at 0x...>" — an address we cannot and must not match. No fixture
		// drives str() on an OrderedMap; fail loudly instead of guessing.
		return "<utils.turnstile.OrderedMap object at 0x0>"
	default:
		return fmt.Sprintf("<%T>", value)
	}
}

// pyRepr is Python's repr() for container items: strings use quote-aware
// single-quote form, everything else like str().
func pyRepr(value any) string {
	if s, ok := value.(string); ok {
		return pyReprString(s)
	}
	return pythonStr(value)
}

// pyReprString renders a string with Python repr's quoting rule: single
// quotes unless the string contains a single quote (and no double quote).
func pyReprString(s string) string {
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		return `"` + pyReprEscape(s, '"') + `"`
	}
	return "'" + pyReprEscape(s, '\'') + "'"
}

func pyReprEscape(s string, quote byte) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\v':
			b.WriteString(`\v`)
		case 0:
			b.WriteString(`\0`)
		default:
			switch {
			case r == rune(quote):
				b.WriteByte('\\')
				b.WriteByte(quote)
			case r < 0x20:
				fmt.Fprintf(&b, `\x%02x`, r)
			default:
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}

// pyEquals is Python == over the value set the port produces: numeric
// types (int, float, bool-as-int) compare cross-type, strings compare by
// value, lists element-wise, objects by content (order-insensitive).
func pyEquals(a, b any) bool {
	if af, isA := pyAsNumber(a); isA {
		bf, isB := pyAsNumber(b)
		if isB {
			return af == bf
		}
		return false
	}
	if as, isA := a.(string); isA {
		bs, isB := b.(string)
		return isB && as == bs
	}
	if al, isA := a.([]any); isA {
		bl, isB := b.([]any)
		if !isB || len(al) != len(bl) {
			return false
		}
		for i := range al {
			if !pyEquals(al[i], bl[i]) {
				return false
			}
		}
		return true
	}
	if ao, isA := a.(pyObj); isA {
		bo, isB := b.(pyObj)
		if !isB || len(ao.pairs) != len(bo.pairs) {
			return false
		}
		for _, pair := range ao.pairs {
			bv, found := bo.find(pair.key)
			if !found || !pyEquals(pair.val, bv) {
				return false
			}
		}
		return true
	}
	if an, isA := a.(json.Number); isA {
		bn, isB := b.(json.Number)
		return isB && an.String() == bn.String()
	}
	switch a.(type) {
	case nil:
		return b == nil
	case *orderedMap:
		return a == b // identity, like Python's default object ==
	}
	return false
}

// pyAsNumber folds Python's numeric tower (int, float, bool-as-int) into
// one float64 comparison.
func pyAsNumber(v any) (float64, bool) {
	switch t := v.(type) {
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case float64:
		return t, true
	}
	return 0, false
}
