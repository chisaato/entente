Status: needs-triage

# path 匹配、多端口容器、健康检查等 label 细节

来源：原设计文档第 5 节与第 11 节（文件已删除，原文在 git 历史）。

第一版的 label 匹配保持最小：host 匹配加 `/` 前缀。path 匹配、多端口容器、健康检查等 label 还没有约定。新增时不能引入隐含默认值（ADR-0002）。
