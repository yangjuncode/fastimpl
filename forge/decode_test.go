package forge

import (
	"strings"
	"testing"
)

// record 是解码测试用的一条记录。
type record struct {
	ID int `json:"id"`
}

// TestDecodeArraysShapes 单个数组、连续多个数组、显式空数组都正常；
// 空输入、空白与 JSON null 不能当成"零条记录"。
func TestDecodeArraysShapes(t *testing.T) {
	ok := []struct {
		name string
		raw  string
		want int
	}{
		{"single", `[{"id":1},{"id":2}]`, 2},
		{"paged", `[{"id":1}][{"id":2},{"id":3}]`, 3},
		{"empty array", `[]`, 0},
		{"empty pages", `[][]`, 0},
	}
	for _, tc := range ok {
		n := 0
		if err := DecodeArrays([]byte(tc.raw), func(page []record) error {
			n += len(page)
			return nil
		}); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if n != tc.want {
			t.Fatalf("%s: decoded %d records, want %d", tc.name, n, tc.want)
		}
	}

	bad := map[string]string{
		"empty input": "",
		"whitespace":  "  \n\t ",
		"null":        "null",
		"null page":   `[{"id":1}]null`,
		"garbage":     "not json",
	}
	for name, raw := range bad {
		err := DecodeArrays([]byte(raw), func(page []record) error { return nil })
		if err == nil {
			t.Fatalf("%s (%q): want an error, got nil", name, raw)
		}
		if name == "empty input" && !strings.Contains(err.Error(), "empty") {
			t.Fatalf("empty input error = %v, want it to mention emptiness", err)
		}
	}
}

// TestDecodeArraysPropagatesCallback 回调错误原样返回（不会被当成 EOF 吞掉）。
func TestDecodeArraysPropagatesCallback(t *testing.T) {
	err := DecodeArrays([]byte(`[{"id":1}]`), func(page []record) error {
		return errCallback
	})
	if err != errCallback {
		t.Fatalf("err = %v, want the callback error", err)
	}
}

// errCallback 是回调里返回的哨兵错误。
var errCallback = &callbackError{"callback failed"}

type callbackError struct{ msg string }

func (e *callbackError) Error() string { return e.msg }
