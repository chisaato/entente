---
Status: accepted
---

# 命令行用 cobra，只接受 pflag 原生写法

命令行入口使用 cobra，只接受 pflag 原生的长选项和短选项，不保留单横线长名（`-config` 等），也不注册 `completion` 子命令。`--config`（`-c`）必填，没有默认路径。`--log-level`（`-l`）默认 `info`。`--check` 默认 false，没有短选项。短选项不引入新的默认值。
