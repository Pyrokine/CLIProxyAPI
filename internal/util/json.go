package util

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
)

// JSONEqual reports whether two JSON byte slices are semantically equal,
// ignoring whitespace and key ordering differences.
func JSONEqual(a, b []byte) bool {
	objA, err := DecodeJSON(a)
	if err != nil {
		return false
	}
	objB, err := DecodeJSON(b)
	if err != nil {
		return false
	}
	return deepEqualJSON(objA, objB)
}

// DecodeJSON 保留数字原文，并要求输入只包含一个完整 JSON 值
func DecodeJSON(raw []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("persisted JSON has multiple values")
		}
		return nil, err
	}
	return value, nil
}

func deepEqualJSON(a, b any) bool {
	switch valA := a.(type) {
	case map[string]any:
		valB, ok := b.(map[string]any)
		if !ok || len(valA) != len(valB) {
			return false
		}
		for key, subA := range valA {
			subB, ok1 := valB[key]
			if !ok1 || !deepEqualJSON(subA, subB) {
				return false
			}
		}
		return true
	case []any:
		sliceB, ok := b.([]any)
		if !ok || len(valA) != len(sliceB) {
			return false
		}
		for i := range valA {
			if !deepEqualJSON(valA[i], sliceB[i]) {
				return false
			}
		}
		return true
	case json.Number:
		valB, ok := b.(json.Number)
		if !ok {
			return false
		}
		if valA == valB {
			return true
		}
		precision := uint(4*(len(valA)+len(valB)) + 64)
		left, _, err := big.ParseFloat(string(valA), 10, precision, big.ToNearestEven)
		if err != nil {
			return false
		}
		right, _, err := big.ParseFloat(string(valB), 10, precision, big.ToNearestEven)
		return err == nil && left.Cmp(right) == 0
	case string:
		valB, ok := b.(string)
		if !ok {
			return false
		}
		return valA == valB
	case bool:
		valB, ok := b.(bool)
		if !ok {
			return false
		}
		return valA == valB
	case nil:
		return b == nil
	default:
		return false
	}
}
