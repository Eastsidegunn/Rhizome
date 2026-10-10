package secretwire

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"sort"
	"strconv"
	"unicode/utf8"
)

// StrictDecode errors (V-J/V-K). The strings are fixed codes: they never carry
// input bytes, key names, numbers or encoding/json error text (S5).
var (
	// ErrMalformed covers V-J 0–5 and missing required keys: bad bytes,
	// syntax, trailing data, duplicate keys, null, wrong JSON type, integer
	// token or range, string grammar.
	ErrMalformed = errors.New("malformed")
	// ErrUnknownField is a key outside the position's exact-case allowlist
	// (V-K, W4).
	ErrUnknownField = errors.New("unknown-field")
)

// maxDepth bounds nesting; every approved shape is far shallower.
const maxDepth = 16

var intTokenRe = regexp.MustCompile(`^(0|[1-9][0-9]{0,15})$`)

type kind int

const (
	kString kind = iota
	kInt
	kBool
	kArray
	kStringList
	kObject
	kUnion
)

// Spec describes the admissible shape of one JSON position. Specs are built
// once with the constructors below and are read-only afterwards.
type Spec struct {
	kind     kind
	str      func(string) bool
	list     func([]string) bool
	min, max uint64
	elem     *Spec
	minN     int
	maxN     int // < 0: unbounded
	fields   map[string]Field
	keys     []string // sorted field names: deterministic first error
	disc     string
	variants map[string]*Spec
}

// Field is one allowlisted object key.
type Field struct {
	Spec     *Spec
	Required bool
}

// Required marks an object key that must be present.
func Required(s *Spec) Field { return Field{Spec: s, Required: true} }

// Optional marks an object key that may be omitted (never null).
func Optional(s *Spec) Field { return Field{Spec: s} }

// String is a JSON string accepted by valid (a §0.1 grammar).
func String(valid func(string) bool) *Spec { return &Spec{kind: kString, str: valid} }

// Enum is a JSON string equal to one of values.
func Enum(values ...string) *Spec {
	set := append([]string(nil), values...)
	return String(func(s string) bool { return in(s, set...) })
}

// Int is a JSON integer token ^(0|[1-9][0-9]{0,15})$ within [min, max].
func Int(min, max uint64) *Spec { return &Spec{kind: kInt, min: min, max: max} }

// Bool is a JSON true/false token.
func Bool() *Spec { return &Spec{kind: kBool} }

// Array is a JSON array of minN..maxN elements of elem (maxN < 0: unbounded).
func Array(elem *Spec, minN, maxN int) *Spec {
	return &Spec{kind: kArray, elem: elem, minN: minN, maxN: maxN}
}

// StringList is a JSON array of strings judged as a whole by valid (e.g.
// ValidScopeTags).
func StringList(valid func([]string) bool) *Spec { return &Spec{kind: kStringList, list: valid} }

// Object is a JSON object whose keys match fields exactly (case-sensitive).
func Object(fields map[string]Field) *Spec {
	s := &Spec{kind: kObject, fields: map[string]Field{}}
	for key, f := range fields {
		s.fields[key] = f
		s.keys = append(s.keys, key)
	}
	sort.Strings(s.keys)
	return s
}

// Union is a JSON object whose shape is chosen by the string value of key:
// variants[value] is applied to the whole object (and must list key itself).
// Variants may be unions, which expresses conditional keys such as
// failureCode-iff-failed.
func Union(key string, variants map[string]*Spec) *Spec {
	return &Spec{kind: kUnion, disc: key, variants: variants}
}

// StrictDecode applies V-J steps 0–6 (SCP §0.1a) to raw against spec. It
// returns nil, ErrMalformed or ErrUnknownField.
func StrictDecode(raw []byte, spec *Spec) error {
	tree, err := parseStrict(raw)
	if err != nil {
		return err
	}
	return validate(tree, spec)
}

// StrictUnmarshal runs StrictDecode and only then decodes raw into v. Because
// the keys were matched exactly and nothing is unknown, encoding/json's
// case-insensitive matching cannot alias a field.
func StrictUnmarshal(raw []byte, spec *Spec, v any) error {
	if err := StrictDecode(raw, spec); err != nil {
		return err
	}
	if json.Unmarshal(raw, v) != nil {
		return ErrMalformed
	}
	return nil
}

// parseStrict performs V-J 0–3: raw byte checks, exactly one JSON value read
// token by token with UseNumber, no duplicate keys at any level, no null. The
// result is a generic tree of map[string]any, []any, string, json.Number,
// bool.
func parseStrict(raw []byte) (any, error) {
	if !utf8.Valid(raw) {
		return nil, ErrMalformed
	}
	for _, b := range raw {
		if b == '\\' || !(b >= 0x20 && b <= 0x7e || b == '\t' || b == '\n' || b == '\r') {
			return nil, ErrMalformed
		}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	v, err := readValue(dec, 0)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, ErrMalformed
	}
	return v, nil
}

func readValue(dec *json.Decoder, depth int) (any, error) {
	if depth > maxDepth {
		return nil, ErrMalformed
	}
	tok, err := dec.Token()
	if err != nil {
		return nil, ErrMalformed
	}
	switch t := tok.(type) {
	case string, json.Number, bool:
		return t, nil
	case json.Delim:
		switch t {
		case '{':
			obj := map[string]any{}
			for dec.More() {
				ktok, err := dec.Token()
				if err != nil {
					return nil, ErrMalformed
				}
				key, ok := ktok.(string)
				if !ok {
					return nil, ErrMalformed
				}
				if _, dup := obj[key]; dup {
					return nil, ErrMalformed
				}
				if obj[key], err = readValue(dec, depth+1); err != nil {
					return nil, err
				}
			}
			if end, err := dec.Token(); err != nil || end != json.Delim('}') {
				return nil, ErrMalformed
			}
			return obj, nil
		case '[':
			arr := []any{}
			for dec.More() {
				v, err := readValue(dec, depth+1)
				if err != nil {
					return nil, err
				}
				arr = append(arr, v)
			}
			if end, err := dec.Token(); err != nil || end != json.Delim(']') {
				return nil, ErrMalformed
			}
			return arr, nil
		}
	}
	return nil, ErrMalformed // null, or a stray closing delimiter
}

// validate performs V-J 4–6 against spec.
func validate(v any, s *Spec) error {
	switch s.kind {
	case kString:
		str, ok := v.(string)
		if !ok || !s.str(str) {
			return ErrMalformed
		}
	case kInt:
		if _, ok := intValue(v, s.min, s.max); !ok {
			return ErrMalformed
		}
	case kBool:
		if _, ok := v.(bool); !ok {
			return ErrMalformed
		}
	case kArray:
		arr, ok := v.([]any)
		if !ok || len(arr) < s.minN || (s.maxN >= 0 && len(arr) > s.maxN) {
			return ErrMalformed
		}
		for _, e := range arr {
			if err := validate(e, s.elem); err != nil {
				return err
			}
		}
	case kStringList:
		arr, ok := v.([]any)
		if !ok {
			return ErrMalformed
		}
		list := make([]string, len(arr))
		for i, e := range arr {
			if list[i], ok = e.(string); !ok {
				return ErrMalformed
			}
		}
		if !s.list(list) {
			return ErrMalformed
		}
	case kObject:
		obj, ok := v.(map[string]any)
		if !ok {
			return ErrMalformed
		}
		for key := range obj {
			if _, ok := s.fields[key]; !ok {
				return ErrUnknownField
			}
		}
		for _, key := range s.keys {
			f := s.fields[key]
			fv, present := obj[key]
			if !present {
				if f.Required {
					return ErrMalformed
				}
				continue
			}
			if err := validate(fv, f.Spec); err != nil {
				return err
			}
		}
	case kUnion:
		obj, ok := v.(map[string]any)
		if !ok {
			return ErrMalformed
		}
		d, ok := obj[s.disc].(string)
		if !ok {
			return ErrMalformed
		}
		variant, ok := s.variants[d]
		if !ok {
			return ErrMalformed
		}
		return validate(obj, variant)
	default:
		return ErrMalformed
	}
	return nil
}

// intValue applies the V-J 4 integer rule: the raw token must match
// ^(0|[1-9][0-9]{0,15})$, then ParseUint and the [min, max] range.
func intValue(v any, min, max uint64) (uint64, bool) {
	num, ok := v.(json.Number)
	if !ok || !intTokenRe.MatchString(string(num)) {
		return 0, false
	}
	n, err := strconv.ParseUint(string(num), 10, 64)
	if err != nil || n < min || n > max {
		return 0, false
	}
	return n, true
}
