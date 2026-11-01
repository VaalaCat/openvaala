package defs

import "time"

// Plugin 是一个启用的插件，加载配置时生成，之后只读。
type Plugin struct {
	Name      string
	Version   string
	Directory string // serve 本机的插件根目录，绝对路径
	// SkillDirectories 是按扫描顺序排列的技能目录绝对路径；没有 skill 时为空。
	SkillDirectories []string
	// InstalledAt 是安装记录里的 installed_at，每次 plugin update 都会变；手动放入的插件为零值。
	InstalledAt time.Time
}
