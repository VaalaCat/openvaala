package desktop

import (
	"embed"
	"encoding/json"
)

//go:embed locales/*.json
var localeFiles embed.FS

// loadTranslations 读取原生中英文目录，界面按本机语言即时选择。
func loadTranslations() map[string]map[string]string {
	// 1. 编译内置资源解码一次，不读取后端配置。
	result := map[string]map[string]string{}
	for _, language := range []string{"en", "zh-CN"} {
		data, _ := localeFiles.ReadFile("locales/" + language + ".json")
		var entries map[string]string
		if err := json.Unmarshal(data, &entries); err != nil {
			panic(err)
		}
		result[language] = entries
	}
	return result
}

// t 返回本机所选语言的原生文案，不翻译服务端原文。
func (d *Desktop) t(key string) string {
	// 1. 缺少所选语言时回退英文，未登记键保留标识。
	if text, found := d.translations[d.preferences.Language][key]; found {
		return text
	}
	if text, found := d.translations["en"][key]; found {
		return text
	}
	return key
}
