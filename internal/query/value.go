package query

import (
	"regexp"
	"strconv"
	"strings"
)

// Value is a normalized field cell. At most one of the pointers is set; all nil
// means the record's field is null. Keeping values typed lets the comparators
// work without reflection.
type Value struct {
	Str  *string
	Num  *float64
	Bool *bool
}

// Str builds a string Value.
func Str(s string) Value { return Value{Str: &s} }

// StrPtr builds a string Value, or a null Value when s is nil.
func StrPtr(s *string) Value {
	if s == nil {
		return Value{}
	}
	v := *s
	return Value{Str: &v}
}

// Num builds a numeric Value.
func Num(f float64) Value { return Value{Num: &f} }

// Int builds a numeric Value from an int.
func Int(i int) Value {
	f := float64(i)
	return Value{Num: &f}
}

// IntPtr builds a numeric Value from an *int, or a null Value when nil.
func IntPtr(i *int) Value {
	if i == nil {
		return Value{}
	}
	f := float64(*i)
	return Value{Num: &f}
}

// Bool builds a boolean Value.
func Bool(b bool) Value { return Value{Bool: &b} }

// BoolPtr builds a boolean Value, or a null Value when b is nil.
func BoolPtr(b *bool) Value {
	if b == nil {
		return Value{}
	}
	v := *b
	return Value{Bool: &v}
}

// IsNull reports whether the value holds no scalar.
func (v Value) IsNull() bool {
	return v.Str == nil && v.Num == nil && v.Bool == nil
}

// ValueString renders a value as a string for group keys and wire encoding. A
// null value renders as "" (matching the FieldMap "empty == absent" convention).
func ValueString(v Value) string {
	switch {
	case v.Str != nil:
		return *v.Str
	case v.Bool != nil:
		if *v.Bool {
			return "true"
		}
		return "false"
	case v.Num != nil:
		return strconv.FormatFloat(*v.Num, 'f', -1, 64)
	default:
		return ""
	}
}

// valueEqual reports equality of two values of the same kind. Mismatched kinds
// (including a null operand) never compare equal.
func valueEqual(a, b Value) bool {
	switch {
	case a.Str != nil && b.Str != nil:
		return *a.Str == *b.Str
	case a.Num != nil && b.Num != nil:
		return *a.Num == *b.Num
	case a.Bool != nil && b.Bool != nil:
		return *a.Bool == *b.Bool
	default:
		return false
	}
}

// cmpHolds applies pred to the ordering of a relative to b (a<b => -1, a==b => 0,
// a>b => 1). A null operand or mismatched kinds yield false.
func cmpHolds(a, b Value, pred func(int) bool) bool {
	c, ok := compareNonNull(a, b)
	if !ok {
		return false
	}
	return pred(c)
}

// compareNonNull orders two non-null values of the same kind. ok is false on a
// null operand or mismatched kinds.
func compareNonNull(a, b Value) (int, bool) {
	switch {
	case a.Str != nil && b.Str != nil:
		return strings.Compare(*a.Str, *b.Str), true
	case a.Num != nil && b.Num != nil:
		switch {
		case *a.Num < *b.Num:
			return -1, true
		case *a.Num > *b.Num:
			return 1, true
		default:
			return 0, true
		}
	case a.Bool != nil && b.Bool != nil:
		switch {
		case !*a.Bool && *b.Bool:
			return -1, true
		case *a.Bool && !*b.Bool:
			return 1, true
		default:
			return 0, true
		}
	default:
		return 0, false
	}
}

// valueIn reports whether v equals any member of set.
func valueIn(v Value, set []Value) bool {
	for _, s := range set {
		if valueEqual(v, s) {
			return true
		}
	}
	return false
}

// matchLike applies an SQL-style LIKE pattern (% = any run, _ = any single char)
// to a string value. A non-string or null value never matches.
func matchLike(v Value, pattern string, caseSensitive bool) bool {
	if v.Str == nil {
		return false
	}
	re, err := likeRegexp(pattern, caseSensitive)
	if err != nil {
		return false
	}
	return re.MatchString(*v.Str)
}

// likeRegexp compiles an SQL LIKE pattern into a whole-string regexp. The (?s)
// flag lets % / _ span newlines, and \A..\z anchor the whole value (not a line),
// so a multi-line field still matches LIKE the way SQL would.
func likeRegexp(pattern string, caseSensitive bool) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("(?s)")
	if !caseSensitive {
		b.WriteString("(?i)")
	}
	b.WriteString(`\A`)
	for _, r := range pattern {
		switch r {
		case '%':
			b.WriteString(".*")
		case '_':
			b.WriteByte('.')
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString(`\z`)
	return regexp.Compile(b.String())
}
