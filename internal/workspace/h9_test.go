package workspace

import (
	"reflect"
	"testing"
)

var _ = intentRequest{Intent: Intent{}, Actor: "actor"}

func TestHTTPDecoderHasNoVerifiedFieldFRRHZ153(t *testing.T) {
	typeOf := reflect.TypeOf(intentRequest{})
	if typeOf.NumField() != 2 || typeOf.Field(0).Name != "Intent" || typeOf.Field(1).Name != "Actor" {
		t.Fatalf("HTTP decoder fields changed: %v", typeOf)
	}
}
