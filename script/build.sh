#!/usr/bin/env bash
set -euo pipefail

# usage 输出构建参数和使用示例。
usage() {
  # 1. 输出帮助正文。
  cat <<'EOF'
用法: script/build.sh [选项]

默认构建当前工作区（包含未提交修改），目标为本机系统和架构；
先用 pnpm 构建 www 页面，再打包进二进制。
  --matrix          默认矩阵: linux,darwin × amd64,arm64
  --os LIST         系统，逗号分隔；覆盖默认系统列表
  --arch LIST       架构，逗号分隔；覆盖默认架构列表
  --branch REF      构建本地已有的分支、远端引用、tag 或 commit
                    仅使用该提交的文件，不自动 fetch，不切换当前分支
  --version VALUE   指定程序版本；默认取所构建提交最近可达的 Git tag
                    没有 tag 时使用 dev-<短提交号>
  --output DIR      输出目录（默认: 仓库根目录/dist）
  --skip-frontend   不构建页面，沿用 www/out/web 里已有的文件；没有时二进制不带页面
  -h, --help        显示帮助

示例:
  script/build.sh
  script/build.sh --matrix
  script/build.sh --os linux --arch amd64,arm64
  script/build.sh --os darwin --arch arm64 --branch main --output /tmp/dist
  script/build.sh --matrix --branch main --version v0.2.0
  script/build.sh --skip-frontend

使用 CGO_ENABLED=0 交叉编译；Go 支持的目标不代表项目依赖也支持。
当前项目包含 Unix 专用调用，Windows 暂不在默认矩阵中。
输出文件为 openvaala-<系统>-<架构>（Windows 添加 .exe）。
EOF
}

# fail 输出错误并以状态码 1 退出。
fail() {
  # 1. 向标准错误输出错误并退出。
  printf '错误: %s\n' "$*" >&2
  exit 1
}

# 1. 准备仓库路径和默认参数。
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
output_dir="$repo_root/dist"
os_list=''
arch_list=''
source_ref=''
version=''
matrix=false
skip_frontend=false

# 2. 读取构建目标、源码引用和输出参数。
while (($#)); do
  case "$1" in
    --matrix) matrix=true; shift ;;
    --skip-frontend) skip_frontend=true; shift ;;
    --os|--arch|--branch|--output|--version)
      (($# >= 2)) && [[ -n "$2" && "$2" != --* ]] || fail "$1 需要参数"
      case "$1" in
        --os) os_list=$2 ;;
        --arch) arch_list=$2 ;;
        --branch) source_ref=$2 ;;
        --output) output_dir=$2 ;;
        --version) version=$2 ;;
      esac
      shift 2
      ;;
    -h|--help) usage; exit 0 ;;
    *) fail "未知参数: $1（使用 --help 查看帮助）" ;;
  esac
done

# 3. 检查构建命令，补全并校验目标系统和架构。
command -v go >/dev/null || fail '找不到 go 命令'
if ! "$skip_frontend"; then
  command -v pnpm >/dev/null || fail '找不到 pnpm 命令（只构建 Go 时使用 --skip-frontend）'
fi
if "$matrix"; then
  os_list=${os_list:-linux,darwin}
  arch_list=${arch_list:-amd64,arm64}
else
  os_list=${os_list:-$(go env GOHOSTOS)}
  arch_list=${arch_list:-$(go env GOHOSTARCH)}
fi

for list in "$os_list" "$arch_list"; do
  [[ "$list" =~ ^[a-z0-9]+(,[a-z0-9]+)*$ ]] || fail "无效的目标列表: $list"
done
IFS=, read -r -a target_systems <<< "$os_list"
IFS=, read -r -a target_architectures <<< "$arch_list"
supported_targets=$(go tool dist list)
for target_os in "${target_systems[@]}"; do
  for target_arch in "${target_architectures[@]}"; do
    grep -Fxq "$target_os/$target_arch" <<< "$supported_targets" ||
      fail "Go 不支持目标: $target_os/$target_arch"
  done
done

# 4. 登记临时目录清理和退出信号处理。
source_dir=$repo_root
temp_dir=''
# cleanup 删除构建指定提交时创建的临时目录。
cleanup() {
  # 1. 删除已创建的临时目录。
  if [[ -n "$temp_dir" ]]; then
    rm -rf -- "$temp_dir"
  fi
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# 5. 使用当前工作区，或将指定提交导出到临时目录。
if [[ -n "$source_ref" ]]; then
  commit=$(git -C "$repo_root" rev-parse --verify --end-of-options "${source_ref}^{commit}") ||
    fail "找不到提交: $source_ref（远端分支请先 git fetch）"
  temp_dir=$(mktemp -d)
  source_dir="$temp_dir/source"
  mkdir -p -- "$source_dir"
  git -C "$repo_root" archive "$commit" | tar -x -C "$source_dir"
  printf '源码: %s (%s)\n' "$source_ref" "$commit"
else
  commit=$(git -C "$repo_root" rev-parse --verify HEAD)
  printf '源码: 当前工作区 %s\n' "$repo_root"
fi

# 6. 读取并校验版本号。
if [[ -z "$version" ]]; then
  version=$(git -C "$repo_root" describe --tags --abbrev=0 "$commit" 2>/dev/null) ||
    version="dev-$(git -C "$repo_root" rev-parse --short "$commit")"
fi
[[ "$version" =~ ^[a-zA-Z0-9][a-zA-Z0-9._+/-]*$ ]] || fail "无效版本号: $version"
printf '版本: %s\n' "$version"

# 7. 在所构建的源码目录里构建页面，输出到 www/out/web 供 go:embed 打包。
if ! "$skip_frontend"; then
  printf '构建页面: %s\n' "$source_dir/www"
  pnpm --dir "$source_dir/www" install --frozen-lockfile
  pnpm --dir "$source_dir/www" build
fi

# 8. 逐个构建目标并输出生成文件的位置。
mkdir -p -- "$output_dir"
output_dir=$(cd -- "$output_dir" && pwd)
cd -- "$source_dir"
build_workspace=${GOWORK:-auto}
if [[ -n "$source_ref" ]]; then
  build_workspace=off
fi
for target_os in "${target_systems[@]}"; do
  for target_arch in "${target_architectures[@]}"; do
    binary="$output_dir/openvaala-$target_os-$target_arch"
    if [[ "$target_os" == windows ]]; then
      binary+=.exe
    fi
    printf '构建 %s/%s → %s\n' "$target_os" "$target_arch" "$binary"
    CGO_ENABLED=0 GOOS="$target_os" GOARCH="$target_arch" GOWORK="$build_workspace" \
      go build -mod=readonly -trimpath -ldflags "-X github.com/VaalaCat/openvaala/pkg/defs.buildVersion=$version" \
        -o "$binary" ./cmd/openvaala
  done
done
printf '构建完成: %s\n' "$output_dir"
