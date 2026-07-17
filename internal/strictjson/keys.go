package strictjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

var ErrDuplicateObjectKey = errors.New("JSON object contains a duplicate key")

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
				} else if rootValues != 0 {
					return errors.New("JSON contains multiple root values")
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
				current.expectKey = false
				continue
			}
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

func matchingDelimiters(opening, closing json.Delim) bool {
	return (opening == '{' && closing == '}') || (opening == '[' && closing == ']')
}
