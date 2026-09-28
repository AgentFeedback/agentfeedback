package schema

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// decimal is a JSON number reduced to sign, significant digits and a power of
// ten: the value is digits × 10^exp. digits has no leading or trailing zeros
// and is empty for zero, so -0, 0.0 and 0e5 are the same decimal. The
// exponent is an exact integer kept as decimal digits, so a spelling of any
// length compares exactly and costs only its own length.
type decimal struct {
	neg    bool
	digits string
	exp    bigInt
}

// bigInt is a signed integer as a decimal digit string: mag has no leading
// zeros and is "" for zero, which is never negative.
type bigInt struct {
	neg bool
	mag string
}

func bigFromDigits(digits string, neg bool) bigInt {
	mag := strings.TrimLeft(digits, "0")
	if mag == "" {
		return bigInt{}
	}
	return bigInt{neg: neg, mag: mag}
}

func bigFromInt(n int64) bigInt {
	return bigFromDigits(strconv.FormatInt(absInt64(n), 10), n < 0)
}

func absInt64(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

func (a bigInt) isZero() bool { return a.mag == "" }

func (a bigInt) sign() int {
	switch {
	case a.isZero():
		return 0
	case a.neg:
		return -1
	}
	return 1
}

func (a bigInt) negate() bigInt {
	if a.isZero() {
		return a
	}
	return bigInt{neg: !a.neg, mag: a.mag}
}

func (a bigInt) add(b bigInt) bigInt {
	if a.neg == b.neg {
		return bigInt{neg: a.neg, mag: addMag(a.mag, b.mag)}
	}
	switch cmpMag(a.mag, b.mag) {
	case 0:
		return bigInt{}
	case 1:
		return bigInt{neg: a.neg, mag: subMag(a.mag, b.mag)}
	}
	return bigInt{neg: b.neg, mag: subMag(b.mag, a.mag)}
}

func (a bigInt) sub(b bigInt) bigInt { return a.add(b.negate()) }

// small returns the value when it fits comfortably in an int64.
func (a bigInt) small() (int64, bool) {
	if len(a.mag) > 18 {
		return 0, false
	}
	n, _ := strconv.ParseInt("0"+a.mag, 10, 64)
	if a.neg {
		n = -n
	}
	return n, true
}

// cmpMag compares two magnitudes without leading zeros.
func cmpMag(a, b string) int {
	if len(a) != len(b) {
		if len(a) < len(b) {
			return -1
		}
		return 1
	}
	return strings.Compare(a, b)
}

func addMag(a, b string) string {
	if len(a) < len(b) {
		a, b = b, a
	}
	out := make([]byte, len(a)+1)
	carry := byte(0)
	for i := 0; i < len(a); i++ {
		d := a[len(a)-1-i] - '0' + carry
		if i < len(b) {
			d += b[len(b)-1-i] - '0'
		}
		out[len(out)-1-i] = d%10 + '0'
		carry = d / 10
	}
	out[0] = carry + '0'
	return strings.TrimLeft(string(out), "0")
}

// subMag returns a-b for magnitudes with a >= b.
func subMag(a, b string) string {
	out := make([]byte, len(a))
	borrow := byte(0)
	for i := 0; i < len(a); i++ {
		d := int(a[len(a)-1-i]-'0') - int(borrow)
		if i < len(b) {
			d -= int(b[len(b)-1-i] - '0')
		}
		borrow = 0
		if d < 0 {
			d += 10
			borrow = 1
		}
		out[len(out)-1-i] = byte(d) + '0'
	}
	return strings.TrimLeft(string(out), "0")
}

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
	var exp bigInt
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
		exp = bigFromDigits(s[es:i], neg)
	}
	if i != len(s) {
		return decimal{}, false
	}
	digits := strings.TrimLeft(intPart+frac, "0")
	trimmed := strings.TrimRight(digits, "0")
	if trimmed == "" {
		return decimal{}, true
	}
	// Shift the exponent down by the fraction and up by the trailing zeros.
	d.digits = trimmed
	d.exp = exp.add(bigFromInt(int64(len(digits)-len(trimmed)) - int64(len(frac))))
	return d, true
}

func (d decimal) isZero() bool { return d.digits == "" }

// isInt reports whether the value has a zero fraction (JSON Schema integer).
func (d decimal) isInt() bool { return d.isZero() || !d.exp.neg }

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
	// Same sign, both non-zero. The order of magnitude is len(digits)+exp;
	// compare the two exactly, then the significant digits (a proper prefix
	// is the smaller value, since trailing zeros are gone).
	order := d.exp.sub(o.exp).add(bigFromInt(int64(len(d.digits)) - int64(len(o.digits))))
	c := order.sign()
	if c == 0 {
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
