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

func TestValidateExactRootObject(t *testing.T) {
	allowed := []string{"schema_version", "events"}
	tests := []struct {
		name       string
		raw        string
		depth      int
		unexpected bool
		valid      bool
	}{
		{name: "exact fields", raw: `{"schema_version":1,"events":[{"Payload":true}]}`, depth: 1, valid: true},
		{name: "case variant", raw: `{"schema_version":1,"Events":[]}`, depth: 1, unexpected: true},
		{name: "unknown field", raw: `{"schema_version":1,"events":[],"extra":true}`, depth: 1, unexpected: true},
		{name: "nested arbitrary field", raw: `{"schema_version":1,"events":[{"Unknown":true}]}`, depth: 1, valid: true},
		{name: "root array", raw: `[]`, depth: 1},
		{name: "root scalar", raw: `true`, depth: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateExactRootObject([]byte(test.raw), test.depth, allowed...)
			if test.unexpected && !errors.Is(err, ErrUnexpectedObjectKey) {
				t.Fatalf("error = %v, want unexpected key", err)
			}
			if test.valid && err != nil {
				t.Fatalf("unexpected error = %v", err)
			}
			if !test.valid && !test.unexpected && err == nil {
				t.Fatal("invalid root unexpectedly succeeded")
			}
		})
	}
	if err := ValidateExactRootObject([]byte(`{}`), 1); err != nil {
		t.Fatalf("empty exact object = %v", err)
	}
	if err := ValidateExactRootObject([]byte(`{"unexpected":true}`), 1); !errors.Is(err, ErrUnexpectedObjectKey) {
		t.Fatalf("zero-field schema error = %v, want unexpected key", err)
	}
}

func TestValidateExactRootObjectRequired(t *testing.T) {
	fields := []string{"schema_version", "complete"}
	if err := ValidateExactRootObjectRequired([]byte(`{"schema_version":1,"complete":false}`), 1, fields...); err != nil {
		t.Fatalf("complete required object = %v", err)
	}
	if err := ValidateExactRootObjectRequired([]byte(`{"schema_version":1}`), 1, fields...); !errors.Is(err, ErrMissingObjectKey) {
		t.Fatalf("missing field error = %v, want missing key", err)
	}
}
