# 使用量 SQLite 容量

[English](usage-sqlite-capacity.md) | 中文

## 适用范围

CLIProxyAPI 使用 SQLite 保存使用量明细和聚合桶，本文面向需要规划容量、保留周期、备份与迁移的运维人员

本文公式只用于规划，不构成磁盘占用或性能保证，请以所用 release 附带的配置示例和管理接口行为为准

## 物理文件

默认 SQLite 存储在 `usage-data/` 下使用三个文件：

- `events.db`：主数据库
- `events.db-wal`：预写日志
- `events.db-shm`：共享内存辅助文件

管理接口 `/usage/db-size` 返回三者合计大小，容量检查不能只计算 `events.db`

## 容量模型

估算导入或较长保留周期时使用以下维度：

- `N`：忽略重复 fingerprint 后接受的 event 行数
- `H`：按模型、凭据、API Key 和提供商区分的 UTC 小时桶数量
- `D`：按模型、凭据、API Key 和提供商区分的 UTC 天桶数量
- 模型、凭据、API Key、来源和提供商的去重维度行

```text
SQLite rows = N events + H hour_bucket + D day_bucket + dimension rows
H <= min(N, hours * models * credentials * api_keys * providers)
D <= min(N, days  * models * credentials * api_keys * providers)
```

规划 JSON 迁移时，除非预先知道存在重复 fingerprint，否则使用解析出的请求明细数量作为 `N` 的上限；不同输入格式结构不同，JSON
字节数不能作为稳定的行数倍数

为 WAL 增长、页面变化和索引预留空间：

```text
max-db-size-mb >= ceil(estimated_sqlite_bytes * 1.2 / 1048576)
```

20% 只作为初始余量，正式设置生产上限前，应在有代表性的导入过程中观察 `/usage/db-size`

## 导入与导出限制

当前构建中的便携使用量导入和导出限制为 100 MiB，超出上限的输入会被拒绝，不会截断

使用时间、模型、API Key 或凭据过滤条件，把大型导出分成较小范围；同时为数据库、WAL、导入临时工作和导出文件保留足够空间

## 容量上限与保留周期

`usage-retention.max-db-size-mb` 作用于 `events.db`、WAL 和 SHM 的合计大小，只有在其他容量控制已经存在时才设置为 `0`

达到上限后，新的使用量明细写入和导入会被拒绝，`/usage/db-size` 会返回 warning 和 capped 状态；数据库不会自动分成多个 SQLite
文件

可以使用 `usage-retention.days` 清理较早的请求明细，也可以在确认可用存储后提高上限，或把较早时间范围导出到 CLIProxyAPI
之外归档；聚合计数和请求明细的保留行为可能不同，依赖清理达到特定容量目标前，需要核对所用 release 的配置行为

## 备份与迁移

服务仍在写入时，不要只复制 `events.db`，已提交的数据可能仍位于 WAL 文件中

使用以下方式之一：

1. 使用管理接口的导出和导入流程生成便携备份
2. 停止写入后复制完整 SQLite 文件集合
3. 使用与部署环境兼容的 SQLite online backup 工具

便携备份经过认证加密，导入通常使用与导出相同的管理凭证；两个实例的管理凭证不同时，在导出和导入请求中提供同一个专用
`X-Usage-Backup-Key`，该值应与备份文件分开传输，并从 shell 历史和日志中删除

便携备份 v4 保存不可变的请求时计价凭证；导入端继续兼容 v3，v3 历史事件没有请求时凭证。导入与历史重算可以更新当前价格目录推导金额，但不能修改已保存的凭证

迁移后先检查导入结果、`/usage/db-size`、已配置容量上限和有代表性的使用量汇总，再删除源备份

## 运维检查清单

- 统计存储时同时计算 `events.db`、WAL 和 SHM
- 大型导入前估算 `N`、`H` 和 `D`
- 便携导入和导出文件保持在支持的限制内
- 先预留至少 20% 工作空间，再根据测量结果调整
- 监控 `/usage/db-size` 的 warning 和 capped 状态
- 存储耗尽前配置保留周期或外部归档
- 不把 API Key、管理凭证或备份密钥写入已提交文件和诊断日志
- 删除旧副本前核对导入数量和汇总结果
