// Package docs 保存 OpenVaala 的使用文档，同一份 Markdown 供文档站和内置 skill 使用。
package docs

import "embed"

// Files 是随程序发布的 OpenVaala 文档，内置 Executor 把它们作为 openvaala skill 放进 /skills/openvaala。
//
//go:embed SKILL.md index.md getting-started guides configuration reference zh
var Files embed.FS
