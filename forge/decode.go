package forge

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// DecodeArrays 解码 --paginate 的输出：它既可能是单个 JSON 数组，
// 也可能是连续多个数组。空输入、空白、JSON null 都不是"零条评论"，
// 只有显式空数组才算有效空列表——读取结果空或畸形时宁可报错。
func DecodeArrays[T any](raw []byte, each func([]T) error) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	arrays := 0
	for {
		var page []T
		err := dec.Decode(&page)
		if errors.Is(err, io.EOF) {
			if arrays == 0 {
				return fmt.Errorf("paged output is empty; expected at least one JSON array")
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("decode paged output: %w", err)
		}
		arrays++
		if page == nil {
			return fmt.Errorf("paged output contains JSON null where an array was expected")
		}
		if err := each(page); err != nil {
			return err
		}
	}
}
