// Package skill 解析通用 Agent Skills 格式（SKILL.md），按目录优先级生成去重、排序的 skill 目录，
// 显式引用时由 Session 读取正文；模型也可按 Path 用通用工具读取。
package skill

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/VaalaCat/openvaala/pkg/telemetry"
	"github.com/VaalaCat/openvaala/pkg/workspace"
	"gopkg.in/yaml.v3"
)

// Skill 是可供 Agent 选用的目录项，保存来源工作区供接受 slash 引用时读取正文。
type Skill struct {
	Name        string
	Description string
	// Path 是 SKILL.md 在所在 Executor 文件系统中的绝对路径；记录扫描时的路径，不解析符号链接。
	// 例如本机 skill 为 /home/me/.claude/skills/pdf/SKILL.md，openvaala 内置 skill 为 /skills/openvaala/SKILL.md。
	Path string
	// Executor 是 SKILL.md 所在的 Executor，例如内置 skill 为 "openvaala"。
	Executor  string
	workspace workspace.Workspace
}

// SkillDirectory 指向递归包含多个技能的目录，或直接包含 SKILL.md 的单个技能目录，经 Workspace 读取。
// ExecutorID 是目录所在的 Executor，写入这个目录里每个 Skill 的 Executor；Strict 为 true 时目录读不了或 skill 格式错误会让加载失败，否则跳过。
type SkillDirectory struct {
	Workspace  workspace.Workspace
	ExecutorID string
	Path       string
	Strict     bool
	// NamePrefix 加在目录里每个 skill 名前面：插件目录为 "<插件名>:"，其他目录为空。
	NamePrefix string
}

// maxSkillFileBytes 是读取单个 SKILL.md 的上限。
const maxSkillFileBytes = 1 << 20

// skillFrontmatter 是 SKILL.md 开头 YAML 中本包使用的字段；其他字段忽略。
type skillFrontmatter struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

// plainSkillNamePattern 是不带插件前缀的 skill 名：小写字母、数字，以单个连字符分隔。
var plainSkillNamePattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// skillNamePattern 是引用 skill 时的合法名字：可带一个 "<插件名>:" 前缀。
var skillNamePattern = regexp.MustCompile(`^([a-z0-9]+(-[a-z0-9]+)*:)?[a-z0-9]+(-[a-z0-9]+)*$`)

// LoadSkills 扫描并加载 Skill 目录。
func LoadSkills(ctx context.Context, directories []SkillDirectory) ([]*Skill, error) {
	// 1. 按优先级依次扫描各工作区目录；目录不存在时跳过，严格目录读取失败时返回错误，其余失败记日志。
	skillsByName := make(map[string]*Skill)
	for _, directory := range directories {
		directory.Path = filepath.Clean(directory.Path)
		pendingDirectories := []string{directory.Path}
		for len(pendingDirectories) > 0 {
			directoryPath := pendingDirectories[0]
			pendingDirectories = pendingDirectories[1:]
			entries, err := directory.Workspace.ReadDirectory(ctx, directoryPath)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				if directory.Strict {
					return nil, fmt.Errorf("read skill directory %s: %w", directoryPath, err)
				}
				telemetry.GetLogger(ctx).Warn("skill directory skipped",
					"path", directoryPath, "openvaala.executor.id", directory.ExecutorID, "error", err)
				continue
			}
			// 2. 包含 SKILL.md 时读取该技能，附属目录不再扫描；同名项保留先加载的结果。
			if slices.ContainsFunc(entries, func(entry *workspace.DirectoryEntry) bool { return entry.Name == "SKILL.md" }) {
				skillPath := filepath.Join(directoryPath, "SKILL.md")
				loaded, err := readSkill(ctx, directory, skillPath, filepath.Base(directoryPath))
				if err != nil {
					if directory.Strict {
						return nil, fmt.Errorf("skill %s: %w", skillPath, err)
					}
					telemetry.GetLogger(ctx).Warn("skill skipped",
						"path", skillPath, "openvaala.executor.id", directory.ExecutorID, "error", err)
					continue
				}
				if _, found := skillsByName[loaded.Name]; !found {
					skillsByName[loaded.Name] = loaded
				}
				continue
			}
			// 3. 递归普通目录；符号链接只作为单个技能目录读取，不继续遍历链接下的子目录。
			if directoryPath != directory.Path {
				if metadata, err := directory.Workspace.GetMetadata(ctx, directoryPath, false); err != nil || metadata.IsSymlink {
					continue
				}
			}
			for _, entry := range entries {
				entryPath := filepath.Join(directoryPath, entry.Name)
				if entry.IsFile {
					continue
				}
				if metadata, err := directory.Workspace.GetMetadata(ctx, entryPath, true); err == nil && metadata.IsDirectory {
					pendingDirectories = append(pendingDirectories, entryPath)
				}
			}
		}
	}
	// 4. 按 Skill 名称排序返回目录项。
	return slices.SortedFunc(maps.Values(skillsByName), func(a, b *Skill) int {
		return strings.Compare(a.Name, b.Name)
	}), nil
}

// readSkill 读取并校验一份 Skill 目录项。
func readSkill(ctx context.Context, directory SkillDirectory, skillPath, directoryName string) (*Skill, error) {
	// 1. 读取最多 1 MiB 的文件并解析 YAML 文件头。
	content, err := directory.Workspace.ReadFile(ctx, skillPath, 0, maxSkillFileBytes)
	if err != nil {
		return nil, err
	}
	frontmatter, err := parseFrontmatter(string(content.Content))
	if err != nil {
		return nil, err
	}
	// 2. 缺省名称使用目录名，校验引用名称，折叠描述空白并检查描述非空。
	if frontmatter.Name == "" {
		frontmatter.Name = directoryName
	}
	if !isPlainNameValid(frontmatter.Name) {
		return nil, fmt.Errorf("name %q must use lowercase letters, digits and single hyphens", frontmatter.Name)
	}
	description := strings.Join(strings.Fields(frontmatter.Description), " ")
	if description == "" {
		return nil, errors.New("description must not be empty")
	}
	// 3. 保存带目录前缀的名称、扫描路径、执行环境和来源工作区。
	return &Skill{
		Name:        directory.NamePrefix + frontmatter.Name,
		Description: description,
		Path:        skillPath,
		Executor:    directory.ExecutorID,
		workspace:   directory.Workspace,
	}, nil
}

// parseFrontmatter 解析 SKILL.md 开头的 YAML 文件头。
func parseFrontmatter(content string) (*skillFrontmatter, error) {
	// 1. 去掉 BOM，检查首行是 ---。
	lines := strings.SplitAfter(strings.TrimPrefix(content, "\ufeff"), "\n")
	if strings.TrimRight(lines[0], "\r\n") != "---" {
		return nil, errors.New("SKILL.md must start with a --- frontmatter line")
	}
	// 2. 找到下一行 --- 并解析其间 YAML；没有结束行时返回错误。
	for index := 1; index < len(lines); index++ {
		if strings.TrimRight(lines[index], "\r\n") != "---" {
			continue
		}
		frontmatter := &skillFrontmatter{}
		if err := yaml.Unmarshal([]byte(strings.Join(lines[1:index], "")), frontmatter); err != nil {
			return nil, fmt.Errorf("invalid frontmatter: %w", err)
		}
		return frontmatter, nil
	}
	return nil, errors.New("SKILL.md frontmatter is not closed")
}

// GetContent 从来源工作区读取 Skill 文件当前的完整内容。
func (s *Skill) GetContent(ctx context.Context) (string, error) {
	// 1. 从来源工作区读取包含 YAML 文件头的完整正文，失败时附上名称和路径。
	text, err := workspace.GetFileText(ctx, s.workspace, s.Path)
	if err != nil {
		return "", fmt.Errorf("read skill %q (%s): %w", s.Name, s.Path, err)
	}
	return text, nil
}

// isPlainNameValid 不带插件前缀的 Skill 名称是否合法？
func isPlainNameValid(name string) bool {
	// 1. 匹配整个名称，只允许小写字母、数字和单个分隔连字符。
	return plainSkillNamePattern.MatchString(name)
}

// IsNameValid Skill 引用名称是否合法？
func IsNameValid(name string) bool {
	// 1. 按小写字母、数字和单个分隔连字符匹配名称，最多带一个插件名前缀。
	return skillNamePattern.MatchString(name)
}

// SkillInfo 是补全列表的显示信息，不含正文和文件读取依赖。
type SkillInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Path        string `json:"path"`
	Executor    string `json:"executor"`
}

// GetSkillsByNames 按目录顺序选择指定名称的 Skill。
func GetSkillsByNames(names []string, catalog []*Skill) []*Skill {
	// 1. 未指定名称时返回全部目录项，否则按目录顺序筛选。
	if len(names) == 0 {
		return catalog
	}
	var selected []*Skill
	for _, listed := range catalog {
		if slices.Contains(names, listed.Name) {
			selected = append(selected, listed)
		}
	}
	return selected
}
