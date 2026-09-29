package schema

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// keywordCodes maps the keyword that fails to the warning code it yields.
// conformance/warnings.json carries the same table; a test compares them.
var keywordCodes = map[string]string{
	"type":          "type_mismatch",
	"minimum":       "out_of_range",
	"maximum":       "out_of_range",
	"enum":          "out_of_range",
	"minLength":     "out_of_range",
	"minItems":      "out_of_range",
	"maxItems":      "out_of_range",
	"maxProperties": "out_of_range",
	"format":        "invalid_format",
	"x-max-bytes":   "too_long",
	"x-recommended": "missing_recommended",
	"x-unique-by":   "duplicate_<member>",
	"properties":    "unknown_field",
}

const (
	codeNoSchema             = "no_schema"
	codeUnknownSchemaVersion = "unknown_schema_version"
)

func code(keyword string) string {
	c, ok := keywordCodes[keyword]
	if !ok {
		panic("schema: no warning code for keyword " + keyword)
	}
	return c
}

// Validate guides payload against the schema the server ships for
// (kind, version). kind is an explicit, token-normalised kind; see the
// package documentation for inferred kinds. Without a schema for the kind the
// single detail is no_schema at /kind; with the kind known but not the
// version, unknown_schema_version at /schema_version; in both cases the
// payload is left untouched. Otherwise the result is Guide's.
func Validate(kind string, version uint64, payload map[string]any, placed map[string]bool) []Detail {
	if s, ok := Kind(kind, version); ok {
		return s.Guide(payload, placed)
	}
	if _, known := reg.kinds[kind]; known {
		return []Detail{{
			Code:    codeUnknownSchemaVersion,
			Pointer: "/schema_version",
			Message: fmt.Sprintf("no schema for %s version %d", kind, version),
		}}
	}
	return []Detail{{
		Code:    codeNoSchema,
		Pointer: "/kind",
		Message: fmt.Sprintf("no schema for kind %q; payload stored unvalidated", kind),
	}}
}

// Guide validates payload against s as a guide, rewriting it in place where
// the schema transforms and returning one Detail per violation; pointers
// start at /payload. placed names the top-level members inference put there
// (moved members, moved, context_raw, context_overflow, value); they were
// reported once already and get no unknown_field. Guide never fails for a
// payload made of the decoder's JSON values.
func (s *Schema) Guide(payload map[string]any, placed map[string]bool) []Detail {
	g := &guide{placed: placed}
	g.apply(s, payload, "/payload", true)
	return g.out
}

type guide struct {
	out    []Detail
	placed map[string]bool
}

func (g *guide) warn(code, pointer, message string) {
	g.out = append(g.out, Detail{Code: code, Pointer: pointer, Message: message})
}

// apply validates v in place and returns the possibly transformed value so
// the caller holding a scalar can store it.
func (g *guide) apply(s *Schema, v any, pointer string, root bool) any {
	actual := typeOf(v)
	if s.types != nil && !matchesType(actual, s.types) {
		g.warn(code("type"), pointer, fmt.Sprintf("expected %s, got %s", strings.Join(s.types, " or "), actual))
		return v
	}
	switch actual {
	case "string":
		str := v.(string)
		switch {
		case s.normalize == "token":
			str = Token(str)
		case s.trim:
			str = Trim(str)
		}
		if s.maxBytes > 0 && len(str) > s.maxBytes {
			if s.onViolation == "truncate" {
				var cut bool
				if str, cut = TruncateBytes(str, s.maxBytes); cut && s.trim && s.normalize != "token" {
					str = Trim(str) // a trimmed member is trimmed again after a cut
				}
			} else {
				g.warn(code("x-max-bytes"), pointer, fmt.Sprintf("longer than %d bytes", s.maxBytes))
			}
		}
		if s.minLength != nil && utf8.RuneCountInString(str) < *s.minLength {
			g.warn(code("minLength"), pointer, fmt.Sprintf("shorter than %d code points", *s.minLength))
		}
		g.checkEnum(s, str, pointer)
		if s.format == "date-time" {
			if _, ok := ParseDateTime(str); !ok {
				g.warn(code("format"), pointer, "not an RFC 3339 date-time")
			}
		}
		return str
	case "integer", "number":
		spelling, _ := numberOf(v)
		if d, ok := parseDecimal(spelling); ok {
			if s.minimum != nil && d.cmp(s.minimum.dec) < 0 {
				g.warn(code("minimum"), pointer, "below "+s.minimum.spelling)
			}
			if s.maximum != nil && d.cmp(s.maximum.dec) > 0 {
				g.warn(code("maximum"), pointer, "above "+s.maximum.spelling)
			}
		}
		g.checkEnum(s, v, pointer)
		return v
	case "array":
		arr := v.([]any)
		if s.minItems != nil && len(arr) < *s.minItems {
			g.warn(code("minItems"), pointer, fmt.Sprintf("fewer than %d items", *s.minItems))
		}
		if s.maxItems != nil && len(arr) > *s.maxItems {
			g.warn(code("maxItems"), pointer, fmt.Sprintf("more than %d items", *s.maxItems))
		}
		// An items schema without any keyword is skipped, as the reference
		// does (an empty dict is false there); one with keywords applies.
		if s.items != nil && !s.items.empty {
			for i := range arr {
				arr[i] = g.apply(s.items, arr[i], pointer+"/"+strconv.Itoa(i), false)
			}
		}
		if s.uniqueBy != "" {
			seen := map[string]bool{}
			for i, item := range arr {
				obj, ok := item.(map[string]any)
				if !ok {
					continue
				}
				value, ok := obj[s.uniqueBy].(string)
				if !ok {
					continue
				}
				if seen[value] {
					g.warn("duplicate_"+s.uniqueBy, pointer+"/"+strconv.Itoa(i),
						fmt.Sprintf("%s %q appears more than once", s.uniqueBy, value))
				}
				seen[value] = true
			}
		}
		return arr
	case "object":
		obj := v.(map[string]any)
		if s.maxProperties != nil && len(obj) > *s.maxProperties {
			g.warn(code("maxProperties"), pointer, fmt.Sprintf("more than %d members", *s.maxProperties))
		}
		for _, name := range slices.Sorted(maps.Keys(obj)) {
			child := pointer + "/" + EscapeToken(name)
			switch sub, listed := s.properties[name]; {
			case listed:
				obj[name] = g.apply(sub, obj[name], child, false)
			case root && g.placed[name]:
				// Placed by inference and reported once already.
			default:
				g.warn(code("properties"), child, fmt.Sprintf("%q is not a member the schema knows", name))
			}
		}
		for _, name := range s.recommended {
			if _, present := obj[name]; !present {
				g.warn(code("x-recommended"), pointer+"/"+EscapeToken(name), name+" is recommended")
			}
		}
		return obj
	}
	// null and boolean: only enum applies. Like the reference, enum is not
	// applied to arrays or objects; no shipped schema declares one there.
	g.checkEnum(s, v, pointer)
	return v
}

func (g *guide) checkEnum(s *Schema, v any, pointer string) {
	if s.enum == nil {
		return
	}
	for _, allowed := range s.enum {
		if jsonEqual(v, allowed) {
			return
		}
	}
	g.warn(code("enum"), pointer, "not one of "+enumText(s.enum))
}

func enumText(values []any) string {
	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = fmt.Sprintf("%v", v)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func matchesType(actual string, wanted []string) bool {
	for _, w := range wanted {
		if w == actual || (actual == "integer" && w == "number") {
			return true
		}
	}
	return false
}
