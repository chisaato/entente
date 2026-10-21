---
Status: accepted
---

# Docker watcher 是 level-triggered 的

相关 Docker 事件只把状态标成「脏」，250ms 防抖后做一次完整 `ContainerList`，而不是按单条事件增量改模型。先订阅事件再 list，避免漏事件。事件流断开时按 1s→30s 退避重连，期间保留最后一次成功同步的结果。

启用了 Docker 来源时，第一次同步完成前不发布快照。否则控制平面重启后会先推一份不含 Docker 路由的快照。Envoy 在这期间继续用旧配置（实测：重启控制平面期间 40/40 请求返回 200）。
