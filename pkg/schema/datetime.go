package schema

import "time"

// ParseDateTime parses the contract's RFC 3339 section 5.6 subset: ASCII
// digits, T and Z in either case, no space separator, no leap second (:60
// does not parse), any offset with -00:00 read as UTC, fractional digits
// beyond six truncated. The instant is returned in UTC at microsecond
// precision; a result outside years 0001 to 9999 does not parse.
func ParseDateTime(text string) (time.Time, bool) {
	p := 0
	num := func(width int) (int, bool) {
		if p+width > len(text) {
			return 0, false
		}
		v := 0
		for k := 0; k < width; k++ {
			c := text[p+k]
			if !isDigit(c) {
				return 0, false
			}
			v = v*10 + int(c-'0')
		}
		p += width
		return v, true
	}
	expect := func(c byte) bool {
		if p < len(text) && text[p] == c {
			p++
			return true
		}
		return false
	}
	year, ok1 := num(4)
	ok2 := expect('-')
	month, ok3 := num(2)
	ok4 := expect('-')
	day, ok5 := num(2)
	if !(ok1 && ok2 && ok3 && ok4 && ok5) || p >= len(text) || (text[p] != 'T' && text[p] != 't') {
		return time.Time{}, false
	}
	p++
	hour, ok1 := num(2)
	ok2 = expect(':')
	minute, ok3 := num(2)
	ok4 = expect(':')
	second, ok5 := num(2)
	if !(ok1 && ok2 && ok3 && ok4 && ok5) {
		return time.Time{}, false
	}
	micro := 0
	if expect('.') {
		fs := p
		for p < len(text) && isDigit(text[p]) {
			p++
		}
		if p == fs {
			return time.Time{}, false
		}
		frac := text[fs:p]
		if len(frac) > 6 {
			frac = frac[:6]
		}
		for k := 0; k < 6; k++ {
			micro *= 10
			if k < len(frac) {
				micro += int(frac[k] - '0')
			}
		}
	}
	offset := 0 // seconds to add to reach UTC
	switch {
	case expect('Z'), expect('z'):
	case p < len(text) && (text[p] == '+' || text[p] == '-'):
		sign := text[p]
		p++
		oh, ok1 := num(2)
		ok2 := expect(':')
		om, ok3 := num(2)
		if !(ok1 && ok2 && ok3) || oh > 23 || om > 59 {
			return time.Time{}, false
		}
		offset = (oh*60 + om) * 60
		if sign == '+' {
			offset = -offset
		}
	default:
		return time.Time{}, false
	}
	if p != len(text) {
		return time.Time{}, false
	}
	if year < 1 || month < 1 || month > 12 || day < 1 || day > daysIn(year, month) ||
		hour > 23 || minute > 59 || second > 59 {
		return time.Time{}, false
	}
	t := time.Date(year, time.Month(month), day, hour, minute, second, micro*1000, time.UTC)
	t = t.Add(time.Duration(offset) * time.Second)
	if y := t.Year(); y < 1 || y > 9999 {
		return time.Time{}, false
	}
	return t, true
}

func daysIn(year, month int) int {
	switch month {
	case 2:
		if year%4 == 0 && (year%100 != 0 || year%400 == 0) {
			return 29
		}
		return 28
	case 4, 6, 9, 11:
		return 30
	}
	return 31
}

// FormatDateTime writes the stored form, YYYY-MM-DDTHH:MM:SS.ffffffZ: UTC,
// six fractional digits (truncated), the year always four digits. Callers
// pass instants within years 0001 to 9999, which is all ParseDateTime yields.
func FormatDateTime(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000000Z")
}

// NormalizeDateTime parses text with ParseDateTime and returns the stored
// form, or false when it does not parse.
func NormalizeDateTime(text string) (string, bool) {
	t, ok := ParseDateTime(text)
	if !ok {
		return "", false
	}
	return FormatDateTime(t), true
}
