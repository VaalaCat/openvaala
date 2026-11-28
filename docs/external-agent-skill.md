---
name: openvaala
description: 仅在以下情况使用：环境变量 OPENVAALA_SESSION_ID 有值（当前运行在 OpenVaala 中），或用户明确要求操作 OpenVaala。其他情况不要使用。
---

1. 先检查 OPENVAALA_SESSION_ID：
   有值：用 openvaala MCP 的 `get_skill` 工具读取 `openvaala` 查看协作方式（或运行 `openvaala skill get openvaala`）。
   没有值且用户没提 OpenVaala：停止，不要使用本 skill。
2. 用户明确要求、但不在 OpenVaala 中：`openvaala` 读取本机 runtime-server.yaml 连接，需要 `openvaala serve` 正在运行。
