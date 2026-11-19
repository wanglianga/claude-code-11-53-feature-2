# 架构与失败模式

## 1. 数据流

```
              ┌─────────────────────────── 区域 (每个区域独立 PostgreSQL 数据库) ───────────────────────────┐
 农机/网关 ──▶ │ POST /regions/{r}/ingest                                                                   │
              │   ┌─ 单事务 ─────────────────────────────────────────────────────────────────────────────┐ │
              │   │ operation_events (唯一: device_serial, segment_id)  ← 本地权威记录                    │ │
              │   │ journal_entries   (seq 连续, 无空洞)                     ← 本地记录连续性证据         │ │
              │   │ outbox_events     (seq 连续; 每操作拆 summary/trajectory/alarm 事件)                    │ │
              │   │ ingest_receipts   (batch_id + 内容 SHA-256)                                           │ │
              │   └───────────────────────────────────────────────────────────────────────────────────────┘ │
              │ 聚合时在区域内计算 ─▶ metric_snapshots + outbox(aggregate_metric)                          │
              └───────────────────────────────────────┬────────────────────────────────────────────────────┘
                                                      │ 复制编排器 (政策过滤 + 逐事件)
                                                      ▼
              ┌────────────────────────── 控制面库 agrisync_control ──────────────────────────────────────┐
              │ residency_policies  ← 逐类别出境判定                                                       │
              │ replica_op_summaries / replica_trajectories / replica_alarms / replica_metrics            │
              │   status = unconfirmed → confirmed                                                       │
              │ replication_cursors (last_seq, confirmed_seq)   replication_runs (含 fault/错误)          │
              │ dr_registry (primary/standby/active, epoch, last_confirmed_seq)                          │
              │ outbound_requests (审批, version)   aggregate_runs / aggregate_run_regions (coverage)     │
              │ deliveries (artifact 原始字节 + sha256)   audit_events (哈希链)                           │
              └──────────────────────────────────────────────────────────────────────────────────────────┘
```

关键不变式：

| 不变式 | 实现 |
| --- | --- |
| 受限原始轨迹不离境 | 区域库独存原始点；复制逐事件按 `residency_policies` 过滤；交付前再扫描实际字节 |
| 集团可见 = 已确认 | 副本 `unconfirmed` 不参与聚合；聚合只读 `status='confirmed'` 的 `replica_metrics` |
| 去重不重复计面积 | 主键 `(source_region, entity_id)` + 唯一索引 `(source_region, device_serial, segment_id)`，全部 upsert |
| 本地记录连续 | `journal_entries` 序列在写事务内按行锁分配，`/journal/verify` 用 `generate_series` 找空洞 |
| 切换只从已确认位置续传 | failover 删除 `unconfirmed`、把游标回绕到 `dr_registry.last_confirmed_seq` |
| 撤回即阻断 | deliver 事务外重新读取审批与政策；`artifact` 仅在全部检查通过时生成 |
| 报表可自证 | artifact 按字节存 `text` + SHA-256；`/deliveries/{id}/verify` 复算并校验审计条目；`/audit/verify` 校验整链 |

## 2. 失败模式与行为

### 2.1 区域断网（链路故障）

- 写入路径完全不触碰控制面 → 现场仍可连续记账，`journal_head == journal_entries`。
- outbox 事件堆积（`pending_outbox` 可见），但本地记录、去重、回执全部正常。
- 集团聚合：该区域 `replica_metrics` 无本窗口确认快照 → `excluded_regions` + `stale_regions`，合计不含该区；
  `data_cutoff` 取所有纳入区域的最早确认截止点。
- 链路恢复后 `POST /replication/run` 从 `last_seq` 继续，一次扫完积压；重叠重传按片段去重。

### 2.2 复制重试 / 至少一次投递

- `copy_fail`：apply 事务回滚，游标不动，`retry_count++`，`replication_runs` 记录失败原因。
- `confirm_fail`：数据已写但 ack 丢失 → 行存在且保持 `unconfirmed`（对集团不可见），游标/DR 不前进。
- 崩溃后重投：相同 `(region, entity_id)` upsert；聚合只读确认快照，不会二次累加面积。

### 2.3 主备切换

- 只能从 `last_confirmed_seq` 续传；`unconfirmed` 行被删除（旧主未确认缓存绝不视为已发布）。
- 本地日志与本地记账不截断，新主继续接受写入，`epoch` 递增并写入 `dr_events` 与审计。
- 续传后再次确认，`replica_*` 回到与区域库一致的行数/面积。

### 2.4 审批在聚合运行中被撤回

- 聚合运行时记录 `approval_version`；交付时重新读取审批：状态、版本、有效期、区域范围、指标范围。
- 任一不一致 → `blocked`；`artifact=null`，不写任何出境内容；审计为 `delivery.blocked`（`deny`）。
- 指标超出审批范围在聚合阶段即 `403`；区域超出审批范围在聚合阶段被范围裁剪，交付阶段再校验一次。

### 2.5 审计链

- 链式哈希：`hash = sha256(canonical{seq, occurred_at(μs), actor, role, action, subject, region, decision, detail, prev_hash})`。
- `detail` 入库前 JSON 归一化（等价于 jsonb 往返后的形态），避免写读差异。
- 任何行被改写：`prev_hash` 与上一条不符或内容哈希不符 → `/audit/verify` 返回 `valid=false`、`broken_at=<seq>`。

## 2.6 生育阶段对齐

```
区域库（每区独立）                                       控制面库
  season_rules / phenology_varieties / phenology_plots     replica_phenology_products
  sowing_facts(rev) / stage_calendars / weather_stations   （区域粒度阶段产品，无田块标识）
  station_links / temperature_observations(rev)            phenology_comparisons
  stage_observations / phenology_runs(不可变, 指纹)          phenology_deliveries（原始字节+哈希）
        │                                                        ▲
        └─ POST /regions/{r}/phenology/runs 派生 ─ outbox(phenology_stage) ─ 复制（按政策）┘
```

关键点：

| 约束 | 实现 |
| --- | --- |
| 同一季节规则版本绑定全链路 | 所有证据表以 `rule_id` 为键；运行与比较都携带 `season_id/rule_version`；比较跨规则即 `mixed_season_rule` 排除 |
| 两条可解释路线 | `das`：播种日 + 品种日数阈值（无则回退农艺日历）；`gdd`：温度站累计 GDD（基温+阈值） |
| 不确定即待定，禁止凑图 | 品种不明/多作物/站不适用/缺气象/缺 GDD 参数/证据矛盾/缺样使阶段跨越 → 计入 `pending`，永不并入阶段 |
| 补种按分段 | 同地块多播次的每个 `segment` 独立推导，`segmented_plots` 与 `reseeded_area_ha` 显式记录 |
| 运行不可变 | 每次派生写一条 `phenology_runs` + `evidence_fingerprint`（所读播种事实/气象观测版本）；更正只产生新运行 |
| 只出区域粒度 | `phenology_stage` 产品仅含面积/待定/证据范围；交付前对确切字节做受限字段扫描；`contains_raw_trajectories=false` |
| 交付前复核 | `POST /phenology/comparisons/{id}/deliver` 重读审批状态/版本/有效期/区域范围并扫描坐标字段 |

## 3. 权限模型
| 角色 | 可做 | 不可做 |
| --- | --- | --- |
| `region_operator`（绑定单一区域） | 本区域上传、本区域状态/作业/日志校验、本区域复制 | 跨区读写、全局复制、切换、聚合、审批 |
| `group_analyst` | 提交出境申请、聚合、交付 | 审批/撤回、切换、重置、读取区域原始记录 |
| `approver` | 审批、驳回、撤回 | 提交申请、交付、聚合 |
| `auditor` | 读审计、校验 | 任何写操作 |
| `admin` | 链路开关、全局复制、主备切换、演示重置 | 仍受交付前置检查约束（不能绕过审计） |

自批被禁止：`decide` 需要 `approver` 角色，`group_analyst` 调用返回 `403`。
