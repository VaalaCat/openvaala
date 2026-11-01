package e2e

import (
	"image/color"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// imageUnsupportedPlaceholder 是模型不能接收图片时，请求里替换图片的那句文字。
const imageUnsupportedPlaceholder = "[图片已省略：当前模型不能接收图片]"

// TestReadImage 检查 read_file 读到的图片会进入模型请求，不能接收图片的模型收到占位文字。
func TestReadImage(t *testing.T) {
	// 1. 构建服务并列出模型的图片接收能力。
	binary := buildOpenVaala(t)
	for _, test := range []struct {
		name string
		// modelInput 是写进 default 模型 capabilities.input 的值；为空时沿用 writeE2EConfig 的 [text, image]。
		modelInput string
		// wantImage 图片消息是否要带 data:image/png;base64, 开头的图片？为假时整个请求里都不能有图片，图片消息改成占位文字。
		wantImage bool
	}{
		{
			name:      "small_png_reaches_model",
			wantImage: true,
		},
		{
			name:       "unsupported_model_gets_placeholder",
			modelInput: "[text]",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			// 1. 配置模型并在工作区写入图片。
			model := newE2EModel(t)
			config := writeE2EConfig(t, model.server.URL)
			if test.modelInput != "" {
				path := filepath.Join(filepath.Dir(config), "models", "local.yaml")
				payload, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				rewritten := strings.Replace(string(payload), "input: [text, image]", "input: "+test.modelInput, 1)
				if rewritten == string(payload) {
					t.Fatalf("%s = %q, want it to hold the default model's capabilities.input to rewrite", path, payload)
				}
				if err := os.WriteFile(path, []byte(rewritten), 0600); err != nil {
					t.Fatal(err)
				}
			}
			workspace := t.TempDir()
			imagePath := filepath.Join(workspace, "shot.png")
			if err := os.WriteFile(imagePath, newE2EPNG(t, color.White), 0600); err != nil {
				t.Fatal(err)
			}

			// 2. 启动对话，让模型调用 read_file 读取图片。
			wire := startAppServer(t, binary, config, "stdio")
			wire.call(t, "initialize", codexClientInfo, nil)
			threadID := (&codexTurnFixture{
				wire:  wire,
				model: model,
			}).startThread(t, workspace, nil).Thread.ID
			requestID := wire.request(t, "turn/start", map[string]any{"threadId": threadID, "input": []any{codexTextInput("look at shot.png")}})
			model.next(t).reply <- e2eModelReply{
				tool:      "read_file",
				arguments: `{"path":"` + imagePath + `"}`,
			}
			second := model.next(t)

			// 3. 检查工具消息后的图片消息、说明文字和图片内容。
			messages := getModelMessageParts(second.body)
			assistantIndex := slices.IndexFunc(messages, func(message modelMessageParts) bool { return message.role == "assistant" })
			if assistantIndex < 0 || len(messages) != assistantIndex+3 || messages[assistantIndex+1].role != "tool" || messages[assistantIndex+2].role != "user" {
				t.Fatalf("model request messages = %+v, want the tool message and then the image user message right after the assistant message", messages)
			}
			imageMessage := messages[assistantIndex+2]
			if intro := "以下是工具调用 call-1 读到的图片：" + imagePath; !strings.Contains(imageMessage.text, intro) {
				t.Errorf("image message text = %q, want it to contain %q", imageMessage.text, intro)
			}
			if hasPlaceholder := strings.Contains(imageMessage.text, imageUnsupportedPlaceholder); hasPlaceholder == test.wantImage {
				t.Errorf("image message text = %q, contains placeholder %v, want %v", imageMessage.text, hasPlaceholder, !test.wantImage)
			}
			var requestImageURLs []string
			for _, message := range messages {
				requestImageURLs = append(requestImageURLs, message.imageURLs...)
			}
			if test.wantImage {
				if !slices.Equal(requestImageURLs, imageMessage.imageURLs) || len(requestImageURLs) != 1 || !strings.HasPrefix(requestImageURLs[0], "data:image/png;base64,") {
					t.Errorf("model request image URLs = %q, want only the image message to carry one data:image/png;base64, URL", requestImageURLs)
				}
			} else if len(requestImageURLs) != 0 {
				t.Errorf("model request image URLs = %q, want none for a model that cannot take images", requestImageURLs)
			}

			// 4. 回复模型并检查 Turn 正常结束。
			second.reply <- e2eModelReply{text: "seen"}
			assertCodexTurnCompleted(t, wire.readTurn(t, threadID, requestID), "completed")
		})
	}
}

// modelMessageParts 是假模型请求里的一条消息：角色、全部文字和各个 image_url 段的 URL。
type modelMessageParts struct {
	role, text string
	imageURLs  []string
}

// getModelMessageParts 提取模型请求中每条消息的角色、文字和图片地址。
func getModelMessageParts(body map[string]any) []modelMessageParts {
	// 1. 读取消息文字，再按消息顺序收集 image_url 地址。
	texts := getModelMessageTexts(body)
	messages, _ := body["messages"].([]any)
	parts := make([]modelMessageParts, len(texts))
	for index, message := range messages {
		parts[index] = modelMessageParts{
			role: texts[index].role,
			text: texts[index].text,
		}
		fields, _ := message.(map[string]any)
		content, _ := fields["content"].([]any)
		for _, part := range content {
			partFields, _ := part.(map[string]any)
			if partFields["type"] != "image_url" {
				continue
			}
			imageURL, _ := partFields["image_url"].(map[string]any)
			url, _ := imageURL["url"].(string)
			parts[index].imageURLs = append(parts[index].imageURLs, url)
		}
	}
	return parts
}
