#!/usr/bin/env bash
set -euo pipefail

# usage 输出双模式桌面构建的参数和产物说明。
usage() {
  # 1. 输出帮助正文。
  cat <<'EOF'
用法: script/build-desktop.sh [选项]

从当前工作区构建包含 WebView、原生界面和完整 CLI 的 MyGo 桌面程序。
默认构建 macOS Universal、Linux amd64 和 arm64；不会上传或发布。

  --platform LIST   逗号分隔的目标（默认: darwin/universal,linux/amd64,linux/arm64）
  --output DIR      产物目录（默认: dist/desktop）
  --skip-frontend   沿用已经构建的 www/out/web；缺失时返回错误
  --debug           保留开发检查功能；不可作为正式发行包
  --skip-dmg        跳过 macOS DMG；非 macOS 主机会自动跳过
  --skip-notarize   跳过 macOS 公证
  -h, --help        显示帮助

例子:
  script/build-desktop.sh
  script/build-desktop.sh --platform linux/amd64 --skip-frontend
  script/build-desktop.sh --platform darwin/universal --skip-notarize

无参数或 app 默认打开 WebView；app --native 打开原生界面。
Linux 的 WebView 需要 WebKitGTK，原生界面只需要 GTK3。
正式构建禁用 native inspector；不包含 e2e automation。
macOS 的签名、公证和 DMG 需要对应系统工具及凭据。
在 Linux 构建 macOS 时仍生成 .app；DMG、签名和实机验证分别说明。
EOF
}

# fail 输出构建失败原因并退出。
fail() {
  # 1. 写入标准错误并返回失败状态。
  printf '错误: %s\n' "$*" >&2
  exit 1
}

# 1. 读取目标、输出目录和构建模式。
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
platforms='darwin/universal,linux/amd64,linux/arm64'
output_dir="$repo_root/dist/desktop"
skip_frontend=false
debug=false
skip_dmg=false
skip_notarize=false
while (($#)); do
  case "$1" in
    --platform|--output)
      (($# >= 2)) && [[ -n "$2" && "$2" != --* ]] || fail "$1 需要参数"
      case "$1" in
        --platform) platforms=$2 ;;
        --output) output_dir=$2 ;;
      esac
      shift 2
      ;;
    --skip-frontend) skip_frontend=true; shift ;;
    --debug) debug=true; shift ;;
    --skip-dmg) skip_dmg=true; shift ;;
    --skip-notarize) skip_notarize=true; shift ;;
    -h|--help) usage; exit 0 ;;
    *) fail "未知参数: $1（使用 --help 查看帮助）" ;;
  esac
done

# 2. 检查目标与构建工具；测试标签不进入发行构建。
command -v go >/dev/null || fail '找不到 go 命令'
IFS=, read -r -a targets <<< "$platforms"
for target in "${targets[@]}"; do
  case "$target" in
    darwin/universal|darwin/amd64|darwin/arm64|linux/amd64|linux/arm64) ;;
    *) fail "不支持的桌面目标: $target" ;;
  esac
done
[[ "$platforms" != *, ]] || fail '目标列表不能以逗号结尾'
case "${GOFLAGS:-}" in
  *e2e*) fail '桌面打包不接受 e2e automation 构建标签' ;;
esac
cd -- "$repo_root"

# 3. 构建供 WebView 和浏览器通过 serve 访问的网页资源。
if ! "$skip_frontend"; then
  command -v pnpm >/dev/null || fail '找不到 pnpm 命令'
  pnpm --dir www install --frozen-lockfile
  pnpm --dir www build
fi
[[ -f www/out/web/index.html ]] || fail '缺少 www/out/web/index.html，请先构建 WebUI'

# 4. 调用同版本 MyGo 构建工具，保留所有 CLI 命令并按平台打包。
mygo_version=$(go list -m -f '{{.Version}}' github.com/egoist/mygo)
[[ "$mygo_version" == v0.7.3 ]] || fail "MyGo 版本必须为 v0.7.3，实际为 $mygo_version"
app_version=$(sed -nE 's/^[[:space:]]*"version"[[:space:]]*:[[:space:]]*"([^"]+)".*/\1/p' mygo.json)
[[ "$app_version" =~ ^[a-zA-Z0-9][a-zA-Z0-9._+-]*$ ]] || fail 'mygo.json 中的 version 不是有效版本号'
export MYGO_LDFLAGS="${MYGO_LDFLAGS:-} -X github.com/VaalaCat/openvaala/pkg/defs.buildVersion=$app_version"
args=(build -platform "$platforms" -o "$output_dir")
if "$debug"; then
  args+=(-debug)
else
  export MYGO_INSPECTOR=0
fi
if [[ "$(go env GOHOSTOS)" != darwin ]]; then
  skip_dmg=true
  skip_notarize=true
  printf '当前主机不是 macOS：生成 .app，但不生成 DMG、不签名或公证。\n'
fi
if "$skip_dmg"; then args+=(-skip-dmg); fi
if "$skip_notarize"; then args+=(-skip-notarize); fi
CGO_ENABLED=0 go run "github.com/egoist/mygo/cmd/mygo@$mygo_version" "${args[@]}"

# 5. 列出实际构建位置，不将交叉编译计为目标系统实机验收。
printf '构建产物: %s\n' "$output_dir"
printf '平台实机验收由使用者安排；请分别核对构建、签名与运行状态。\n'
