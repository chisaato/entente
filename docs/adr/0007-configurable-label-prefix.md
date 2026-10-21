---
Status: accepted
---

# Label 前缀可配置，解析只经过一处

Label 前缀由 `docker.label_prefix` 配置，默认 `entente`，与项目名一致；这是藏得比较深的选项，让同一 Docker 主机上的多个实例用不同前缀、互不干扰。所有 label 解析必须经过 `internal/labels.Parse`，其他地方禁止硬编码前缀字符串。前缀不匹配的容器静默忽略，不暴露。已启用的容器上出现未知的 `<prefix>.*` label 时拒绝该容器，避免 `entrypoint`（少了 s）这类拼写被静默忽略。
