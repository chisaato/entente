---
Status: accepted
---

# xDS 走 Unix Domain Socket，权限边界即信任边界

Envoy 与控制平面同机，xDS 走 Unix Domain Socket。套接字权限（0600，或配置的 `socket_mode`，加上同 owner / group）就是信任边界，不再叠 mTLS 或 token。

## Consequences

UDS 在 umask 0177 下创建，再 chmod 成 `socket_mode`，不会短暂出现比配置更宽的权限。启动时只会删除遗留的 socket 文件；路径上已是普通文件时拒绝启动。
