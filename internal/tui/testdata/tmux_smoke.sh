#!/usr/bin/env bash
# 在真实 tmux 终端启动 openvaala，连接脚本化模型，发送按键并抓屏核对文本。
# 用法：internal/tui/testdata/tmux_smoke.sh；在 120x40 和 60x20 两种尺寸下各跑一遍，任一检查失败时退出码非 0。
# FAKEMODEL_PORT 可以指定 fake 模型的端口，默认由系统分配。
set -euo pipefail

# 清掉 tmux 会继承的 OPENVAALA_* 环境变量，本次运行使用下面生成的配置。
unset "${!OPENVAALA_@}"

ROOT=$(cd "$(dirname "$0")" && git rev-parse --show-toplevel)
SOCKET=ovsmoke-$$
WORK=$(mktemp -d)
MODEL_PID=""
FAILURES=0

# tm 使用私有 socket 调用 tmux，不加载用户配置。
tm() {
	# 1. 使用私有 socket 和空配置执行 tmux。
	tmux -f /dev/null -L "$SOCKET" "$@";
}

# cleanup 停止模型与 tmux，移除临时文件。
cleanup() {
	# 1. 停止 tmux 服务并移除 socket。
	tm kill-server 2>/dev/null || true
	# kill-server 或会话退出之后 socket 文件仍留在磁盘上，删掉它。
	rm -f "${TMUX_TMPDIR:-/tmp}/tmux-$(id -u)/$SOCKET"
	# 2. 停止假模型并移除工作目录。
	[[ -n "$MODEL_PID" ]] && kill "$MODEL_PID" 2>/dev/null || true
	rm -rf "$WORK"
}
trap cleanup EXIT

go build -o "$WORK/openvaala" "$ROOT/cmd/openvaala"
go build -o "$WORK/fakemodel" "$ROOT/internal/tui/testdata/fakemodel"
"$WORK/fakemodel" -addr "127.0.0.1:${FAKEMODEL_PORT:-0}" >"$WORK/fakemodel.log" 2>&1 &
MODEL_PID=$!

# wait_for_model 等待假模型监听并将端口写入 PORT。
wait_for_model() {
	# 1. 在五秒内读取监听端口；进程退出或超时则打印日志并退出。
	local deadline=$((SECONDS + 5))
	while ((SECONDS < deadline)); do
		PORT=$(sed -n 's/^listening on 127\.0\.0\.1:\([0-9]*\)$/\1/p' "$WORK/fakemodel.log")
		[[ -n "$PORT" ]] && return 0
		kill -0 "$MODEL_PID" 2>/dev/null || break
		sleep 0.1
	done
	echo "fakemodel 未能监听：" >&2
	sed 's/^/      | /' "$WORK/fakemodel.log" >&2
	exit 1
}
wait_for_model

CONFIG="$WORK/home/.config/openvaala"
mkdir -p "$CONFIG/agents" "$CONFIG/models" "$CONFIG/skills" "$CONFIG/executors" "$WORK/project"
cat >"$CONFIG/config.yaml" <<EOF
agents:
  directory: agents
models:
  directory: models
skills:
  directory: skills
executors:
  directory: executors
run:
  agent_name: default
EOF
cat >"$CONFIG/agents/default.yaml" <<EOF
name: default
description: 冒烟测试助手
instructions: 你是冒烟测试助手。
model:
  provider: fake
  model: smoke
EOF
cat >"$CONFIG/models/fake.yaml" <<EOF
id: fake
protocol: openai_chat
base_url: http://127.0.0.1:$PORT
api_key: smoke-test-key
models:
  smoke:
    upstream_model: smoke
    display_name: Smoke
    context_window: 32000
    max_output_tokens: 4096
  second:
    upstream_model: second
    display_name: Second
    context_window: 16000
    max_output_tokens: 4096
EOF

# capture 抓取当前画面和滚动历史。
capture() {
	# 1. 抓取当前画面和最近五百行历史，合并折行。
	tm capture-pane -p -J -S -500 -t smoke;
}
# capture_visible 只返回当前可见区域，不含 scrollback。
capture_visible() {
	# 1. 抓取当前可见画面。
	tm capture-pane -p -t smoke;
}
# keys 向测试会话发送按键。
keys() {
	# 1. 向 smoke 会话发送给定按键，参数与 tmux send-keys 相同。
	tm send-keys -t smoke "$@";
}
# escape 发送 Esc 并等待下一次按键。
escape() {
	# 1. 发送 Esc 后等待 0.2 秒；连续两次调用的间隔小于一秒确认窗口、大于五十毫秒合并窗口。
	keys Escape; sleep 0.2;
}
# type_text 向测试会话输入文本。
type_text() {
	# 1. 使用 -l 按字面发送给定文本。
	tm send-keys -t smoke -l "$1";
}
# 以下文本判断先完整抓屏；抓屏失败时返回非零状态。
# has_text 屏幕或 scrollback 里是否出现了 text？
has_text() {
	# 1. 抓屏失败时返回非零，否则在画面和历史中匹配文字。
	local screen; screen=$(capture) || return 1; grep -qF -- "$1" <<<"$screen";
}
# lacks_visible_text 可见区域里是否已经没有 text？
lacks_visible_text() {
	# 1. 抓屏失败时返回非零，否则检查可见画面没有指定文字。
	local screen; screen=$(capture_visible) || return 1; ! grep -qF -- "$1" <<<"$screen";
}
# footer_has_text 可见区域最后一个非空行（footer）里是否有 text？
footer_has_text() {
	# 1. 抓取可见画面，在最后一个非空行匹配文字。
	local screen; screen=$(capture_visible) || return 1; grep -v '^$' <<<"$screen" | tail -n 1 | grep -qF -- "$1";
}
# footer_without_cwd 页脚是否显示 Smoke 且没有工作目录？
footer_without_cwd() {
	# 1. 检查页脚包含 Smoke 且不含 /project。
	footer_has_text "Smoke" && ! footer_has_text "/project";
}
# composer_shows_lines 可见区域是否有相邻两行分别以指定首行和续行开头？
composer_shows_lines() {
	# 1. 抓取可见画面，检查指定首行与续行相邻。
	local screen
	screen=$(capture_visible) || return 1
	awk -v first="› $1" -v second="$2" '
		after_first && index($0, second) == 1 { found = 1 }
		{ after_first = (index($0, first) == 1) }
		END { exit !found }' <<<"$screen"
}
# paste_keeps_indent 粘贴后的第二行是否保留续行提示符两格和原有四格缩进？
paste_keeps_indent() {
	# 1. 检查首行和含六格缩进的第二行相邻。
	composer_shows_lines "第一行" "      缩进的第二行";
}
# ctrl_j_splits_lines Ctrl+J 是否把“上一行”和“下一行”分到了输入框的两行？
ctrl_j_splits_lines() {
	# 1. 检查上下两行分别显示指定文字。
	composer_shows_lines "上一行" "  下一行";
}
# live_area_at_bottom 最后两个非空行是否依次为输入框和指定页脚？
live_area_at_bottom() {
	# 1. 读取最后两个非空行，核对输入提示符和页脚文字。
	local screen
	screen=$(capture_visible) || return 1
	grep -v '^$' <<<"$screen" | tail -n 2 | awk -v footer="$1" '
		NR == 1 { composer = (index($0, "›") == 1) }
		NR == 2 { has_footer = (index($0, footer) > 0) }
		END { exit !(NR == 2 && composer && has_footer) }'
}
# cursor_at_composer 光标是否位于指定列且所在行以输入提示符开头？
cursor_at_composer() {
	# 1. 抓屏并读取光标位置，核对列号和所在行。
	local screen position row
	screen=$(capture_visible) || return 1
	position=$(tm display -p -t smoke '#{cursor_x},#{cursor_y}') || return 1
	row=$(sed -n "$((${position#*,} + 1))p" <<<"$screen")
	[[ "${position%,*}" == "$1" && "$row" == "›"* ]]
}
# footer_on_last_row 终端底行是否含指定页脚文字？
footer_on_last_row() {
	# 1. 读取窗口高度，在底行匹配页脚文字。
	local height
	height=$(tm display -p -t smoke '#{pane_height}') || return 1
	capture_visible | sed -n "${height}p" | grep -Fq "$1"
}
# history_reached tmux 历史是否至少有指定行数？
history_reached() {
	# 1. 读取历史行数并与指定数量比较。
	(($(tm display -p -t smoke '#{history_size}') >= $1));
}
# session_has_exited tmux 会话是否已经结束？openvaala 退出后窗口随之关闭。
session_has_exited() {
	# 1. 检查 smoke 会话已经不存在。
	! tm has-session -t smoke 2>/dev/null;
}
# check_until 轮询条件并报告检查结果。
check_until() {
	# 1. 在指定秒数内反复调用判断函数，成功时打印 PASS 并返回。
	local predicate=$1 text=$2 timeout=$3 label=$4
	local deadline=$((SECONDS + timeout))
	while ((SECONDS < deadline)); do
		if "$predicate" "$text"; then
			echo "PASS  [$SIZE] $label"
			return 0
		fi
		sleep 0.2
	done
	# 2. 超时后打印现场，抓屏失败也继续累计失败数。
	echo "FAIL  [$SIZE] $label：$timeout 秒内没有满足 $predicate \"$text\""
	capture | tail -n 25 | sed 's/^/      | /' || true
	FAILURES=$((FAILURES + 1))
}
# expect 在 timeout 秒内等屏幕或 scrollback 出现 text。
expect() {
	# 1. 轮询画面或历史中的指定文字。
	check_until has_text "$1" "${2:-10}" "${3:-$1}";
}

# start_session 按指定尺寸启动使用同一 HOME 和配置的 tmux 会话。
start_session() {
	# 1. 停止旧 tmux 服务。
	tm kill-server 2>/dev/null || true
	# 2. 使用 screen-256color 创建窗口并启动程序。
	tm start-server \; set -g default-terminal screen-256color \; \
		new-session -d -s smoke -x "$1" -y "$2" -c "$WORK/project" \
		"HOME=$WORK/home $WORK/openvaala --config-file $CONFIG/config.yaml"
}

# run_size 检查指定尺寸下的输入、流式输出、面板、缩放和历史恢复。
run_size() {
	# 1. 按本次尺寸启动会话并等待模型页脚。
	SIZE="$1x$2"
	start_session "$1" "$2"
	expect "Smoke" 15 "启动后 footer 显示模型"

	# 2. 粘贴多行文字，检查换行和缩进后发送。
	tm set-buffer -b smoke $'第一行\n    缩进的第二行'
	tm paste-buffer -p -b smoke -t smoke
	check_until paste_keeps_indent "第一行 / 缩进的第二行" 5 "多行粘贴保留换行和缩进"
	keys Enter
	expect "收到：第一行" 10 "发送多行输入并收到回复"

	# 3. 用 Ctrl+J 换行，再用 Ctrl+C 清空草稿。
	type_text "上一行"; keys C-j; type_text "下一行"
	check_until ctrl_j_splits_lines "上一行 / 下一行" 5 "Ctrl+J 换行"
	keys C-c
	check_until lacks_visible_text "上一行" 5 "Ctrl+C 清空输入框"

	# 4. 运行工具，在展开视图中检查调用与回复，再返回主视图。
	type_text "跑一下工具"; keys Enter
	expect "我先跑个命令" 10 "工具前的回复"
	keys C-o
	expect "展开视图" 5 "运行中 Ctrl+O 进入展开视图"
	expect "echo smoke-ok" 10 "展开视图展开工具调用"
	expect "结论：一切正常" 15 "展开视图里看到工具之后的回复"
	escape
	check_until lacks_visible_text "展开视图" 5 "Esc 退出展开视图"
	expect "└─ bash@local 1 次" 10 "退出后补写工具组汇总"

	# 5. 等待长回复首块定稿，缩放后连按两次 Esc 中断。
	type_text "写个长的"; keys Enter
	expect "冒烟长文" 10 "长回复开始流式输出"
	# 等待与换行同批到达的首段末尾文字，此时首块已写入历史。
	expect "scrollback。" 10 "长回复首个块已定稿进 scrollback"
	tm resize-window -t smoke -x 30 -y "$2"
	sleep 0.2
	tm resize-window -t smoke -x "$1" -y "$2"
	sleep 0.2
	escape; escape
	expect "已中断" 10 "Esc 两次中断"
	# 等待被中断的 Turn 收尾。
	check_until lacks_visible_text "生成中" 10 "中断后 Turn 收尾"

	# 6. 在运行中插话并排队，等待排队输入执行完成。
	type_text "慢慢说"; keys Enter
	expect "生成中" 10 "运行中状态行"
	# 发送含“慢”的插话，假模型继续缓慢回复。
	type_text "慢点补充一句"; keys Enter
	expect "已插入" 5 "steer 已插入"
	type_text "排队的话"; keys Tab
	expect "排队：排队的话" 5 "排队预览"
	expect "收到：排队的话" 30 "排队输入在下一个 Turn 执行"
	# 等待排队输入所在的 Turn 结束。
	check_until lacks_visible_text "生成中" 10 "排队的 Turn 结束"

	keys C-c
	expect "再按 Ctrl+C 退出" 5 "空闲时 Ctrl+C 提示"

	# 7. 检查各个面板的打开与关闭。
	type_text "/context"; keys Enter; expect "上下文详情" 5 "/context 面板"; escape
	keys C-o; expect "展开视图" 5 "Ctrl+O 展开视图"; escape; check_until lacks_visible_text "展开视图" 5 "Esc 关闭展开视图"
	type_text "/model"; keys Enter; expect "Second" 5 "/model 面板列出模型"; escape
	type_text "/agents"; keys Enter; expect "子 Agent" 5 "/agents 面板"; escape
	type_text "/status"; keys Enter; expect "会话信息" 5 "/status 面板"; escape
	type_text "/resume"; keys Enter; expect "恢复对话" 5 "/resume 面板"; escape
	keys "?"; expect "帮助" 5 "? 打开帮助"; escape

	# 8. 缩到 40x12，检查窄屏页脚省略目录，再恢复原尺寸。
	tm resize-window -t smoke -x 40 -y 12
	check_until footer_without_cwd "Smoke" 5 "resize 之后 footer 仍在"
	tm resize-window -t smoke -x "$1" -y "$2"
	# 120 列下检查工作目录恢复；60 列仍省略目录。
	if (($1 >= 120)); then
		check_until footer_has_text "/project" 5 "resize 回原尺寸后 footer 恢复 cwd"
	fi
	local width
	for width in 120 30 120 20 120 30 120 20 120; do
		tm resize-window -t smoke -x "$width" -y "$2"
		sleep 0.2
		check_until footer_on_last_row "ctx" 3 "横向缩放到 $width 列后 footer 贴底"
		check_until cursor_at_composer 2 3 "横向缩放到 $width 列后光标仍在输入框"
	done
	tm resize-window -t smoke -x "$1" -y "$2"

	# 9. 带多行草稿反复缩放，检查草稿和页脚。
	tm set-buffer -b smoke $'draft-start one two three four five six seven eight nine ten eleven twelve thirteen fourteen fifteen\ndraft-end'
	tm paste-buffer -p -b smoke -t smoke
	for width in 30 120 20 120; do
		tm resize-window -t smoke -x "$width" -y "$2"
		sleep 0.2
		check_until footer_on_last_row "ctx" 3 "有折行与多行草稿时缩放到 $width 列后 footer 贴底"
		expect "draft-end" 3 "缩放到 $width 列后保留草稿"
	done
	keys C-c
	tm resize-window -t smoke -x "$1" -y "$2"
	keys C-o
	expect "展开视图" 5 "缩放前打开展开视图"
	tm resize-window -t smoke -x 30 -y "$2"
	escape
	check_until footer_on_last_row "ctx" 3 "展开视图缩放后返回 inline 仍贴底"
	tm resize-window -t smoke -x "$1" -y "$2"
	# 10. 退出当前进程。
	type_text "/quit"; keys Enter
	check_until session_has_exited smoke 5 "/quit 退出"

	# 11. 重启并恢复长对话，等历史超过屏幕后检查输入框、页脚和光标。
	# 等启动时的请求都返回后再打开 /resume 面板，记下这时的历史行数。
	start_session "$1" "$2"
	expect "Smoke" 15 "重新启动"
	sleep 1
	type_text "/resume"; keys Enter; expect "恢复对话" 5 "重新启动后 /resume 面板"
	local history_before
	history_before=$(tm display -p -t smoke '#{history_size}')
	keys Enter
	# scrollback 变多说明写出的历史超出了屏幕。
	check_until history_reached "$((history_before + 1))" 10 "/resume 写出超出屏幕的整段历史"
	sleep 0.5
	check_until live_area_at_bottom "Smoke" 3 "/resume 之后输入框与 footer 仍在屏幕底部"
	check_until cursor_at_composer 2 3 "/resume 之后光标在输入行"
	type_text "/quit"; keys Enter
	check_until session_has_exited smoke 5 "重新启动后 /quit 退出"
}

run_size 120 40
run_size 60 20

if ((FAILURES > 0)); then
	echo "$FAILURES 项检查失败"
	exit 1
fi
echo "全部通过"
