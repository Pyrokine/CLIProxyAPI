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

## 结构化事件读取

原始事件继续使用 schema 9，原始 `metadata` 和不可变的 `pricing_receipt` 保持原值，`event_fields` 为每条保留事件保存一行轻量字段，供
SQL 筛选、搜索、排序、实际速度和 compact 列表读取

SQLite 触发器在实时写入、导入、重算和删除事务中同时维护两张表，费用更新会同步结构化金额，稳定的请求与响应字段沿用已保存值

派生布局单独标识为 `event_fields_version=1`，原始数据格式声明继续为 `usage=1:9:9`，支持 schema 9
的兼容旧候选可以继续读写当前数据库，持久触发器会维护旧候选写入的结构化行，现有 Python SQLite 3.31.1 运维读取工具可加载该
schema

启动时创建轻量表、索引和触发器，历史事件和汇总保持原值，后台每个事务处理最多 128 条历史事件，并在同一事务中提交转换结果和进度，历史升级期间继续接受新写入

升级完成前，查询在同一只读快照中从原始事件读取所需字段，保留精确总数和搜索含义，历史转换完成后使用轻量表

经过管理认证的只读接口 `GET /v0/management/usage/storage-upgrade` 返回 `status`（`idle`、`running`、`complete` 或 `error`）、
`running`，以及精确的 `processed_events`、`total_events` 和 `percent`，计数来自同一数据库快照中的当前保留事件，包含在线新增和删除

保存在 `meta` 中的 `started_at`、`updated_at`、`completed_at` 和 `error`
在重启后继续可读，错误保存失败时，当前进程仍会返回已观察到的错误，并在日志中记录保存失败，旧程序未记录的时间保持缺失

费用重算独立显示，`GET /v0/management/usage/recalculate-costs` 返回该任务已记录的 `started_at`、`updated_at` 和可选
`completed_at`

升级在每页历史处理前检查数据库、WAL 和 SHM 的合计大小，为在线写入预留配置上限的 1%，最多 64
MiB，达到预留边界或容量计量失败时暂停升级并返回错误，已提交的 cursor 和记录保留，处理报错条件后重启继续

在线流量仍会消耗预留空间，升级暂停期间继续观察 `/usage/db-size`

通过只读 SQLite 连接查看已提交的 cursor：

```sql
SELECT key, value FROM meta
WHERE key IN ('schema_version', 'event_fields_version', 'event_fields_cursor');
```

`event_fields_cursor=complete` 表示历史转换完成，整数值表示最后已提交的 event ID，空字符串表示首批历史转换尚未提交

关闭时先取消升级，再排空已接收的使用量写入，重启后从已提交进度继续，升级报错时保留原始事件和已提交批次，未转换事件继续可读，按日志处理报错条件后重启同一数据库

升级和回退程序时保留原始备份，回退到兼容 schema 9 的旧候选时继续使用当前数据库，升级后已接收的记录仍然可用，便携备份 v3/v4
和已封装事件导出的合同保持不变

## 容量模型

估算导入或较长保留周期时使用以下维度：

- `N`：忽略重复 fingerprint 后保留的 event 行数，每条事件另有一行结构化 `event_fields`
- `H`：按模型、凭据、API Key 和提供商区分的 UTC 小时桶数量
- `D`：按模型、凭据、API Key 和提供商区分的 UTC 天桶数量
- `S`：1 ns、10 min、1 h 三种粒度的精确汇总行，包含 auth-index 维度
- `P`：按全量／模型／凭据／API Key 分组的查询投影行，粒度为 1 h、1 d 和固定 30 d UTC 窗口
- 模型、凭据、API Key、auth index、来源和提供商的去重维度行

```text
SQLite rows = N events + N event_fields + H hour_bucket + D day_bucket + S summary_bucket + P summary_projection + dimension rows
S <= 3 * N
```

历史小时／天桶可在请求明细清理后保留，查询投影跟随来源桶，在同一写入、导入、重算和清理事务中更新，容量估算需要包含两张汇总表及其索引，启动回填后检查
`/usage/db-size` 的实际占用

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

## 更改数据目录

通过管理接口保存 `usage-data-dir`，重启进程后应用新目录，保存时会在现有管理状态库中记录来源和目标，重启前连续保存多个目录时，迁移保留最初来源和最终目标

启动时先迁移数据，再打开目标数据库，SQLite 就绪后清除迁移记录，迁移或数据库打开失败时保留记录并输出告警，该进程使用内存统计，检查目标数据后再处理原文件

## 离线校验数据库

在仓库根目录执行校验，输入使用已停止写入的数据库或一致的 SQLite 备份：

```bash
python3 -I scripts/cross_check.py --db /path/to/events.db --prices /path/to/model-prices.json
```

脚本在同一个只读 SQLite 快照中检查 fingerprint、小时与模型／凭据／API Key／提供商的联合分配、API Key 为 NULL 的 OAuth 请求，以及
1 ns、10 min、1 h 三种粒度的精确汇总，精确汇总覆盖五个身份维度和 19 项指标

查询投影与来源小时／天桶独立比较，标为 invalid 的投影由查询入口改用原始桶，离线校验仍检查其来源成员数，并将缓存指标报告为未验证

`--prices` 启用费用与计价凭证校验，接受平面价格 map 或包含 `prices` 字段的模型目录，增加
`--snapshot /path/to/snapshot.json` 可与旧版 JSON 使用量快照比较

当前目录价格变化后，请求时计价凭证继续保留捕获的价格，费用校验接受有效凭证金额，或按输入目录重算的金额，旧派生金额与两者都不同且缺少当时价格快照时，该笔费用保持未验证

| 退出码 | 输出                | 含义                                         |
|--------|---------------------|----------------------------------------------|
| `0`    | `ALL CHECKS PASSED` | 请求的检查全部完成且没有 warning             |
| `1`    | `FAIL:`             | 发现数据不一致                               |
| `2`    | `CHECKS INCOMPLETE` | 历史明细、精确汇总或参考价格不足，检查未完成 |

历史小时／天聚合可在请求明细清理后保留，精确汇总从保留的事件回填，需要的历史精度缺失时，查询返回 HTTP `409` 和
`summary_precision_unavailable`，此时应选择可用的完整聚合桶范围，或保留对应明细，离线校验会将精度缺失报告为检查未完成

## 运维检查清单

- 统计存储时同时计算 `events.db`、WAL 和 SHM
- 大型导入前估算 `N`、`H` 和 `D`
- 便携导入和导出文件保持在支持的限制内
- 先预留至少 20% 工作空间，再根据测量结果调整
- 监控 `/usage/db-size` 的 warning 和 capped 状态
- 存储耗尽前配置保留周期或外部归档
- 不把 API Key、管理凭证或备份密钥写入已提交文件和诊断日志
- 删除旧副本前核对导入数量和汇总结果
