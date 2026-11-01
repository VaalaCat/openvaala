// 本示例加载配置，查询并打印 Agent 和模型。
// 从仓库根目录运行：go run ./examples/load-config
package main

import (
	"fmt"
	"os"
	"slices"

	"github.com/VaalaCat/openvaala/internal/config"
	"github.com/VaalaCat/openvaala/pkg/defs"
)

// main 读取示例配置并打印选中的 Agent 和模型。
func main() {
	// 1. 加载应用设置、Agent 和 Provider。
	cfg, err := config.LoadConfig(&config.LoadConfigOpt{
		Path: "examples/config/config.yaml",
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	// 2. 查询配置中选中的 Agent。
	definition, err := cfg.GetAgent(cfg.Run.AgentName)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	// 3. 按 Agent 的模型引用查找模型。
	ref := defs.ModelRef{
		ProviderID: definition.Model.Provider,
		ModelID:    definition.Model.Model,
	}
	index := slices.IndexFunc(cfg.GetModels(), func(model *defs.ModelDefinition) bool { return model.Ref == ref })
	if index < 0 {
		fmt.Fprintln(os.Stderr, "model not found:", ref)
		os.Exit(1)
	}
	model := cfg.GetModels()[index]

	// 4. 打印 Agent 名称、指令和模型信息。
	fmt.Printf("Agent: %s\nInstructions: %s\nModel: %s (%s)\n", definition.Name, definition.Instructions, model.Ref, model.DisplayName)
}
