package schema

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// decimal is a JSON number reduced to sign, significant digits and a power of
// ten: the value is digits × 10^exp. digits has no leading or trailing zeros
// and is empty for zero, so -0, 0.0 and 0e5 are the same decimal. Exponents
// saturate at ±2^62, far beyond any spelling that matters, so no spelling
// can cost more than its own length.
type decimal struct {
	neg    bool
	digits string
	exp    int64
}

const expSaturation = int64(1) << 62

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// parseDecimal reads an RFC 8259 number spelling.
func parseDecimal(s string) (decimal, bool) {
	var d decimal
	i := 0
	if i < len(s) && s[i] == '-' {
		d.neg = true
		i++
	}
	start := i
	switch {
	case i < len(s) && s[i] == '0':
		i++
	case i < len(s) && s[i] >= '1' && s[i] <= '9':
		for i < len(s) && isDigit(s[i]) {
			i++
		}
	default:
		return decimal{}, false
	}
	intPart := s[start:i]
	frac := ""
	if i < len(s) && s[i] == '.' {
		i++
		fs := i
		for i < len(s) && isDigit(s[i]) {
			i++
		}
		if i == fs {
			return decimal{}, false
		}
		frac = s[fs:i]
	}
	var exp int64
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		i++
		neg := false
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			neg = s[i] == '-'
			i++
		}
		es := i
		for i < len(s) && isDigit(s[i]) {
			i++
		}
		if i == es {
			return decimal{}, false
		}
		exp = parseExponent(s[es:i], neg)
	}
	if i != len(s) {
		return decimal{}, false
	}
	digits := strings.TrimLeft(intPart+frac, "0")
	exp -= int64(len(frac))
	trimmed := strings.TrimRight(digits, "0")
	exp += int64(len(digits) - len(trimmed))
	if trimmed == "" {
		return decimal{}, true
	}
	d.digits = trimmed
	d.exp = exp
	return d, true
}

// parseExponent reads exponent digits, saturating instead of overflowing.
func parseExponent(digits string, neg bool) int64 {
	digits = strings.TrimLeft(digits, "0")
	var v int64
	if len(digits) > 18 {
		v = expSaturation
	} else if len(digits) > 0 {
		v, _ = strconv.ParseInt(digits, 10, 64)
		if v > expSaturation {
			v = expSaturation
		}
	}
	if neg {
		return -v
	}
	return v
}

func (d decimal) isZero() bool { return d.digits == "" }

// isInt reports whether the value has a zero fraction (JSON Schema integer).
func (d decimal) isInt() bool { return d.isZero() || d.exp >= 0 }

// cmp returns -1, 0 or 1 as d is less than, equal to or greater than o.
func (d decimal) cmp(o decimal) int {
	switch {
	case d.isZero() && o.isZero():
		return 0
	case d.isZero():
		if o.neg {
			return 1
		}
		return -1
	case o.isZero():
		if d.neg {
			return -1
		}
		return 1
	case d.neg != o.neg:
		if d.neg {
			return -1
		}
		return 1
	}
	// Same sign, both non-zero: compare magnitudes by order of magnitude,
	// then by significant digits (equal length compares lexicographically;
	// a proper prefix is the smaller value since trailing zeros are gone).
	c := 0
	switch od, oo := int64(len(d.digits))+d.exp, int64(len(o.digits))+o.exp; {
	case od < oo:
		c = -1
	case od > oo:
		c = 1
	default:
		c = strings.Compare(d.digits, o.digits)
	}
	if d.neg {
		return -c
	}
	return c
}

// numberOf returns the JSON spelling of a Go number, or false for anything
// that is not a number. json.Number is the decoder's form; the basic Go
// numeric types are accepted for callers building payloads by hand.
func numberOf(v any) (string, bool) {
	switch n := v.(type) {
	case json.Number:
		return string(n), true
	case float64:
		return strconv.FormatFloat(n, 'g', -1, 64), true
	case float32:
		return strconv.FormatFloat(float64(n), 'g', -1, 32), true
	case int:
		return strconv.Itoa(n), true
	case int64:
		return strconv.FormatInt(n, 10), true
	case uint64:
		return strconv.FormatUint(n, 10), true
	}
	return "", false
}

// typeOf names the JSON type of a value from the decoder's tree.
func typeOf(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	if s, ok := numberOf(v); ok {
		if d, ok := parseDecimal(s); ok && d.isInt() {
			return "integer"
		}
		return "number"
	}
	panic(fmt.Sprintf("schema: unsupported value of type %T; the guide accepts the decoder's JSON tree only", v))
}

// jsonEqual compares two values as JSON: numbers by decimal value, so 2.0
// equals 2; containers member by member.
func jsonEqual(a, b any) bool {
	switch x := a.(type) {
	case nil:
		return b == nil
	case bool:
		y, ok := b.(bool)
		return ok && x == y
	case string:
		y, ok := b.(string)
		return ok && x == y
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !jsonEqual(x[i], y[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, xv := range x {
			yv, ok := y[k]
			if !ok || !jsonEqual(xv, yv) {
				return false
			}
		}
		return true
	}
	as, ok := numberOf(a)
	if !ok {
		return false
	}
	bs, ok := numberOf(b)
	if !ok {
		return false
	}
	ad, ok1 := parseDecimal(as)
	bd, ok2 := parseDecimal(bs)
	return ok1 && ok2 && ad.cmp(bd) == 0
}
