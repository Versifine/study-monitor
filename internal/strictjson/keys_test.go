package strictjson

import (
	"errors"
	"testing"
)

func TestValidateObjectKeys(t *testing.T) {
	tests := []struct {
		name      string
		raw       string
		depth     int
		duplicate bool
		valid     bool
	}{
		{name: "valid nested", raw: `{"a":1,"nested":{"b":"value"},"array":[{"c":true}]}`, valid: true},
		{name: "duplicate root", raw: `{"a":1,"a":2}`, duplicate: true},
		{name: "duplicate escaped key", raw: `{"a":1,"\u0061":2}`, duplicate: true},
		{name: "duplicate nested", raw: `{"nested":{"a":1,"a":2}}`, duplicate: true},
		{name: "duplicate array object", raw: `[{"a":1,"a":2}]`, duplicate: true},
		{name: "root only ignores nested", raw: `{"nested":{"a":1,"a":2}}`, depth: 1, valid: true},
		{name: "trailing value", raw: `{} {}`},
		{name: "malformed", raw: `{"a":`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateObjectKeys([]byte(test.raw), test.depth)
			if test.duplicate && !errors.Is(err, ErrDuplicateObjectKey) {
				t.Fatalf("error = %v, want duplicate key", err)
			}
			if test.valid && err != nil {
				t.Fatalf("unexpected error = %v", err)
			}
			if !test.valid && !test.duplicate && err == nil {
				t.Fatal("invalid JSON unexpectedly succeeded")
			}
		})
	}
}
