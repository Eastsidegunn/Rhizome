package secretwire

import (
	"strings"
	"testing"
)

var testSpec = Object(map[string]Field{
	"name": Required(String(ValidName)),
	"n":    Required(Int(0, 10)),
	"tags": Optional(StringList(ValidScopeTags)),
	"flag": Optional(Bool()),
	"list": Optional(Array(Int(0, 10), 0, 2)),
	"sub":  Optional(Object(map[string]Field{"x": Required(Int(0, 5))})),
})

// FR-RHZ-136: §0.1a V-J steps 0–6 and V-K, each defect rejected with a fixed
// code only.
func TestStrictDecodeFRRHZ136(t *testing.T) {
	ok := []string{
		`{"name":"a","n":3}`,
		" {\t\"name\" : \"a\",\r\n\"n\":0 } \n",
		`{"n":10,"name":"a","tags":["a","b"],"flag":false,"list":[],"sub":{"x":5}}`,
	}
	for _, in := range ok {
		if err := StrictDecode([]byte(in), testSpec); err != nil {
			t.Errorf("valid input rejected: %v", err)
		}
	}
	bad := []struct {
		name string
		in   string
		want error
	}{
		{"empty", ``, ErrMalformed},
		{"broken", `{"name":"a","n":3`, ErrMalformed},
		{"trailing comma", `{"name":"a","n":3,}`, ErrMalformed},
		{"trailing value", `{"name":"a","n":3} {}`, ErrMalformed},
		{"trailing garbage", `{"name":"a","n":3}x`, ErrMalformed},
		{"not object", `["a"]`, ErrMalformed},
		{"duplicate key", `{"name":"a","name":"a","n":3}`, ErrMalformed},
		{"duplicate nested key", `{"name":"a","n":3,"sub":{"x":1,"x":1}}`, ErrMalformed},
		{"case variant key", `{"name":"a","Name":"a","n":3}`, ErrUnknownField},
		{"null value", `{"name":null,"n":3}`, ErrMalformed},
		{"null optional", `{"name":"a","n":3,"flag":null}`, ErrMalformed},
		{"null element", `{"name":"a","n":3,"list":[null]}`, ErrMalformed},
		{"unknown key", `{"name":"a","n":3,"value":"x"}`, ErrUnknownField},
		{"unknown nested key", `{"name":"a","n":3,"sub":{"x":1,"value":"x"}}`, ErrUnknownField},
		{"missing required", `{"name":"a"}`, ErrMalformed},
		{"missing nested required", `{"name":"a","n":3,"sub":{}}`, ErrMalformed},
		{"string as int", `{"name":"a","n":"3"}`, ErrMalformed},
		{"int as string", `{"name":1,"n":3}`, ErrMalformed},
		{"string as bool", `{"name":"a","n":3,"flag":"true"}`, ErrMalformed},
		{"int as bool", `{"name":"a","n":3,"flag":1}`, ErrMalformed},
		{"exponent", `{"name":"a","n":1e1}`, ErrMalformed},
		{"negative", `{"name":"a","n":-1}`, ErrMalformed},
		{"negative zero", `{"name":"a","n":-0}`, ErrMalformed},
		{"leading zero", `{"name":"a","n":01}`, ErrMalformed},
		{"fraction", `{"name":"a","n":1.0}`, ErrMalformed},
		{"out of range", `{"name":"a","n":11}`, ErrMalformed},
		{"17 digits", `{"name":"a","n":10000000000000000}`, ErrMalformed},
		{"too many elements", `{"name":"a","n":3,"list":[1,2,3]}`, ErrMalformed},
		{"grammar", `{"name":"A","n":3}`, ErrMalformed},
		{"tags unsorted", `{"name":"a","n":3,"tags":["b","a"]}`, ErrMalformed},
		{"tags duplicate", `{"name":"a","n":3,"tags":["a","a"]}`, ErrMalformed},
		{"tags empty", `{"name":"a","n":3,"tags":[]}`, ErrMalformed},
		{"tags non-string", `{"name":"a","n":3,"tags":[1]}`, ErrMalformed},
		{"backslash escape a", `{"name":"\u0061","n":3}`, ErrMalformed},
		{"backslash solidus", `{"name":"a\/","n":3}`, ErrMalformed},
		{"non-ascii utf8", `{"name":"é","n":3}`, ErrMalformed},
		{"non-utf8", "{\"name\":\"a\xff\",\"n\":3}", ErrMalformed},
		{"control byte", "{\"name\":\"a\x01\",\"n\":3}", ErrMalformed},
		{"del byte", "{\"name\":\"a\x7f\",\"n\":3}", ErrMalformed},
		{"too deep", strings.Repeat("[", 40) + strings.Repeat("]", 40), ErrMalformed},
	}
	for _, c := range bad {
		if err := StrictDecode([]byte(c.in), testSpec); err != c.want {
			t.Errorf("%s: got %v want %v", c.name, err, c.want)
		}
	}
}

// FR-RHZ-136: StrictUnmarshal only fills v after the shape check passed.
func TestStrictUnmarshalFRRHZ136(t *testing.T) {
	var v struct {
		Name string `json:"name"`
		N    uint64 `json:"n"`
	}
	if err := StrictUnmarshal([]byte(`{"name":"a","n":3}`), testSpec, &v); err != nil || v.Name != "a" || v.N != 3 {
		t.Fatalf("got %+v, %v", v, err)
	}
	v.Name = ""
	if err := StrictUnmarshal([]byte(`{"name":"a","NAME":"b","n":3}`), testSpec, &v); err != ErrUnknownField || v.Name != "" {
		t.Fatalf("case-variant key reached the struct: %+v, %v", v, err)
	}
}

// FR-RHZ-136: unions choose a key set by discriminator, nested unions express
// conditional keys.
func TestStrictUnionFRRHZ136(t *testing.T) {
	spec := Union("k", map[string]*Spec{
		"a": Object(map[string]Field{"k": Required(Enum("a")), "x": Required(Int(0, 1))}),
		"b": Object(map[string]Field{"k": Required(Enum("b"))}),
	})
	cases := map[string]error{
		`{"k":"a","x":1}`: nil,
		`{"k":"b"}`:       nil,
		`{"k":"b","x":1}`: ErrUnknownField,
		`{"k":"a"}`:       ErrMalformed,
		`{"k":"c"}`:       ErrMalformed,
		`{"x":1}`:         ErrMalformed,
		`{"k":1}`:         ErrMalformed,
		`"a"`:             ErrMalformed,
	}
	for in, want := range cases {
		if err := StrictDecode([]byte(in), spec); err != want {
			t.Errorf("%s: got %v want %v", in, err, want)
		}
	}
}
