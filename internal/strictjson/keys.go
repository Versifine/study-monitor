package strictjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

var (
	ErrDuplicateObjectKey  = errors.New("JSON object contains a duplicate key")
	ErrUnexpectedObjectKey = errors.New("JSON object contains an unexpected key")
	ErrMissingObjectKey    = errors.New("JSON object is missing a required key")
)

type container struct {
	delimiter json.Delim
	expectKey bool
	keys      map[string]struct{}
}

// ValidateObjectKeys validates one JSON value and rejects duplicate object keys.
// If checkedContainerDepth is positive, only objects at or above that container
// depth are checked; zero checks every object. The full JSON value is always
// parsed so malformed or trailing input cannot bypass the check.
func ValidateObjectKeys(raw []byte, checkedContainerDepth int) error {
	return validateObjectKeys(raw, checkedContainerDepth, nil, false)
}

// ValidateExactRootObject validates one JSON object, rejects duplicate keys to
// the requested depth, and requires every root key to exactly match one of the
// allowed keys. Matching is deliberately case-sensitive even though
// encoding/json accepts case-insensitive struct field aliases.
func ValidateExactRootObject(raw []byte, checkedContainerDepth int, allowedRootKeys ...string) error {
	return validateObjectKeys(raw, checkedContainerDepth, allowedRootKeys, true)
}

// ValidateExactRootObjectRequired applies the exact-root validation and also
// requires every listed root key to be present. It is intended for versioned
// wire schemas whose fields are all mandatory.
func ValidateExactRootObjectRequired(raw []byte, checkedContainerDepth int, requiredRootKeys ...string) error {
	if err := ValidateExactRootObject(raw, checkedContainerDepth, requiredRootKeys...); err != nil {
		return err
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return fmt.Errorf("decode JSON root object: %w", err)
	}
	for _, key := range requiredRootKeys {
		if _, exists := root[key]; !exists {
			return fmt.Errorf("%w: %q", ErrMissingObjectKey, key)
		}
	}
	return nil
}

func validateObjectKeys(raw []byte, checkedContainerDepth int, allowedRootKeys []string, exactRootObject bool) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	stack := make([]container, 0, 8)
	rootValues := 0

	completeValue := func() error {
		if len(stack) == 0 {
			rootValues++
			if rootValues > 1 {
				return errors.New("JSON contains multiple root values")
			}
			return nil
		}
		parent := &stack[len(stack)-1]
		if parent.delimiter == '{' {
			if parent.expectKey {
				return errors.New("JSON object contains a value without a key")
			}
			parent.expectKey = true
		}
		return nil
	}

	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("decode JSON token: %w", err)
		}

		if delimiter, ok := token.(json.Delim); ok {
			switch delimiter {
			case '{', '[':
				if len(stack) > 0 {
					parent := &stack[len(stack)-1]
					if parent.delimiter == '{' && parent.expectKey {
						return errors.New("JSON object key must be a string")
					}
				} else {
					if rootValues != 0 {
						return errors.New("JSON contains multiple root values")
					}
					if exactRootObject && delimiter != '{' {
						return errors.New("JSON root value must be an object")
					}
				}
				entry := container{delimiter: delimiter, expectKey: delimiter == '{'}
				depth := len(stack) + 1
				if delimiter == '{' && (checkedContainerDepth == 0 || depth <= checkedContainerDepth) {
					entry.keys = make(map[string]struct{})
				}
				stack = append(stack, entry)
			case '}', ']':
				if len(stack) == 0 || !matchingDelimiters(stack[len(stack)-1].delimiter, delimiter) {
					return errors.New("JSON container delimiters do not match")
				}
				current := stack[len(stack)-1]
				if current.delimiter == '{' && !current.expectKey {
					return errors.New("JSON object key has no value")
				}
				stack = stack[:len(stack)-1]
				if err := completeValue(); err != nil {
					return err
				}
			default:
				return errors.New("JSON contains an unexpected delimiter")
			}
			continue
		}

		if len(stack) > 0 {
			current := &stack[len(stack)-1]
			if current.delimiter == '{' && current.expectKey {
				key, ok := token.(string)
				if !ok {
					return errors.New("JSON object key must be a string")
				}
				if current.keys != nil {
					if _, exists := current.keys[key]; exists {
						return ErrDuplicateObjectKey
					}
					current.keys[key] = struct{}{}
				}
				if len(stack) == 1 && exactRootObject && !containsExact(allowedRootKeys, key) {
					return fmt.Errorf("%w: %q", ErrUnexpectedObjectKey, key)
				}
				current.expectKey = false
				continue
			}
		}
		if len(stack) == 0 && exactRootObject {
			return errors.New("JSON root value must be an object")
		}
		if err := completeValue(); err != nil {
			return err
		}
	}

	if len(stack) != 0 {
		return errors.New("JSON container is not closed")
	}
	if rootValues != 1 {
		return errors.New("JSON must contain exactly one value")
	}
	return nil
}

func containsExact(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func matchingDelimiters(opening, closing json.Delim) bool {
	return (opening == '{' && closing == '}') || (opening == '[' && closing == ']')
}
