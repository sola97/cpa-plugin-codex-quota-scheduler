# 额度就绪与错误响应修复上线验证

## 变更范围

- 源码提交：`9f3e11c`（等待额度）和 `b4f2f8b`（启动竞态与错误响应）。
- 插件版本：`0.3.2-sola97.readiness429`。
- Linux amd64 动态库 SHA256：`03c3c83ee98112b0cf9f5c6cf55eb1af8cf4fd05982f3258a8485688842539ff`。
- 本次没有拉取新的 CPA 镜像，没有修改账号保护开关、比例、探测设置或主机连接设置。

启动时 CPA 可能尚未加载认证文件，插件首次取得的权威账号列表因此为空。
此前请求会使用这个未确认状态立即拒绝，同时普通活动唤醒会在五分钟 TTL 内
复用它。现在请求会在后台重新确认尚未确认的权威列表，并等待本轮刷新交接完成
后再判断失败；仍不使用调度候选伪造账号列表，也不启用高风险探测选项。

## 自动测试

WSL Ubuntu 22.04 / Go 1.26.0 上全部通过：

```text
go test ./...
go test -race ./...
go vet ./...
go build -trimpath -buildmode=c-shared ...
```

回归覆盖原请求等待后继续、启动空列表恢复、真实保留线拒绝、已有可用账号时
其他账号异步刷新、并发查询合并、查询失败退避、无法确认列表时不查询、关闭时
释放等待，以及 429/503 的 ABI 和公开 JSON 错误体。

## 实际 HTTP 合约

在两台主机各自的 CPA 镜像中启动独立临时容器，仅挂载虚构认证文件和测试规则。
测试包装库将插件的额度 HTTP 查询替换为固定结果，实际业务 HTTP 请求仍经过
真实 CPA 调度与错误处理。没有使用真实账号，也没有调用模型。容器测试后已移除。

| 场景 | NUC CPA v8.0.31 | Bandwagon CPA v8.0.27 | 公开错误 |
| --- | --- | --- | --- |
| 5 小时剩余 5%，保留 10% | 429，0.534 秒 | 429，0.555 秒 | `usage_limit_reached`，5-hour 提示 |
| 周剩余 5%，保留 10% | 429，0.526 秒 | 429，0.528 秒 | `usage_limit_reached`，weekly 提示 |
| 额度查询返回 503 | 503，0.264 秒 | 503，0.286 秒 | `quota_not_ready`，可重试 |

429 的 `type` 和 `code` 均为 `usage_limit_reached`。两个窗口同时限制时使用
5-hour or weekly 提示，其他保护拒绝使用通用提示，不统一误称 5 小时用尽。

## 生产验证

- NUC 使用同步任务的来源锁进行备份与原子替换，同时重建 CPA 和 TUIC sidecar。
- Bandwagon 由既有同步工具接收，结果为 `changed=1`、`removed=0`、`restart=true`，
  仅插件库发生变化；CPA 与 GOST sidecar 均运行在正确的共享网络命名空间。
- 两端启动日志确认加载上述插件版本，已安装动态库 SHA256 相同。
- NUC 真实 `/v1/responses` 请求返回 200/completed，耗时 1.355 秒。
- Bandwagon 相同模型的真实请求返回 200/completed，耗时 4.658 秒。
- 两端均记录 `scheduler.quota_wait`、`quota.refresh_success`、`scheduler.selected`，
  权威列表 Healthy、启用账号 3 个，三个账号均有额度且没有刷新错误。
- NUC 更新前后配置、Compose 与账号规则的校验值完全一致。
- Bandwagon 配置和 Compose 与更新前备份的校验值完全一致，两端账号规则的
  规范化 JSON 校验值也一致；未替用户开启或调整任何账号规则。
- 原有 NUC 到 Bandwagon 每五分钟同步任务仍保留，本次已手工触发一次正常同步。

备份位置：

```text
NUC: /home/ubuntu/docker-cpa/backups/quota-readiness429-20261016T235728Z
Bandwagon: /root/docker-cpa/backups/nuc-sync-20261016T235809.930661Z
```

本次没有在生产账号上强制制造额度耗尽；429 的完整 HTTP 链路使用上述隔离容器
核验。真实上游故障仍可能产生相应错误，不能保证此后所有请求都成功。等待上限
为 15 秒，当前插件 ABI 不传递客户端取消信号，这一限制未改变。
