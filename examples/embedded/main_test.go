package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

// TestEmbedded 通过公开 API 完成工具调用、持久化查询和关闭重开。
func TestEmbedded(t *testing.T) {
	// 1. 每次运行使用独立资源，验证业务结果和持久用量。
	for _, name := range []string{"first_host", "second_host"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			var output bytes.Buffer
			if err := run(ctx, &output); err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"项目状态：就绪", "status=completed", "Input:20", "Output:10"} {
				if !strings.Contains(output.String(), want) {
					t.Errorf("output %q missing %q", output.String(), want)
				}
			}
		})
	}
}
