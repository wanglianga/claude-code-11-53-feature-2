# AgriSync — 作业数据跨地域同步与出境审计服务

跨国农机运营集团的作业数据平台。播种机、收割机、施肥机在各区域上传作业轨迹、地块面积、油耗与设备告警；
**区域节点本地确认**，复制编排器按**数据类别 × 驻留政策**生成跨区副本，聚合服务**在区域内**计算可出境指标，
出境审计服务在**交付前**重新核验授权与范围，容灾登记册记录每个区域的**主备角色**和**最后已确认复制位置**。

技术栈：Go 1.26 + PostgreSQL 18。后端服务，提供 OpenAPI 3.0.3 文档与内置（离线打包）Swagger UI。

---

## 1. 设计要点

### 1.1 区域隔离是物理的

控制面库 `agrisync_control` 与四个区域库 `agrisync_region_{de_bb,br_mt,us_ia,fr_nor}` 是**独立数据库**。
区域写入只触碰本区域库，控制面库永远不存受限原始轨迹（唯一例外是政策显式允许出境的区域）。因此
"集团只能取得经批准的聚合结果" 不是靠查询过滤，而是靠数据库边界。

### 1.2 本地确认、政策过滤后的异步复制

一次上传在**一个本地事务**里完成：作业记录 + 本地日志（gapless 序列）+ 事务性 outbox + 回执。

- outbox 按类别拆事件：`operation_summary`、`raw_trajectory`、`device_alarm`、`aggregate_metric`、`phenology_stage`；
- 复制按 `residency_policies(region, category, outbound_allowed)` 逐事件过滤，受限轨迹**连控制面都进不去**；
- 副本先落 `unconfirmed`，接收方确认后才变 `confirmed` 并对集团可见，同时推进 DR 登记册的
  `last_confirmed_seq` 与复制游标 `confirmed_seq`；
- 复制失败/链路断开时游标不动，重试从同一位置继续。所有副本写入都是 **upsert**（主键 + `(region, device, segment)` 唯一索引），
  所以"至少一次"重投不会让面积重复累加。

### 1.3 重复上传按"设备序号 + 区域来源"去重

- 片段级：`unique (device_serial, segment_id)`，作用域是接收区域（区域来源隐含在库边界内，跨区副本显式带 `source_region`）；
- 批次级：`(batch_id, 内容 SHA-256)` 相同则返回原始回执（`replayed_batch=true`），内容不同则 `409`；
- 断网补传时重传已接收片段 → `duplicates` 计数，面积/油耗不变。

### 1.4 断网期间农场继续记账，集团统计显式滞后

- 链路关闭（`POST /replication/links/{region}` 或故障注入）不影响本区域写入；
- 聚合报告的 `coverage` 明确列出 `included_regions`、`excluded_regions`、`stale_regions`（延迟区域）、
  每区域 `data_cutoff` / `lag_seconds`、以及所使用的 `replication_seq`（复制版本）；
- 未确认区域不计入合计，只作为"被排除且滞后"出现。

### 1.5 故障切换只从"已确认位置"继续

`POST /dr/{region}/failover`：

1. 提升备节点，`epoch+1`；
2. 删除该区域所有 `unconfirmed` 副本（旧主未确认的缓存**不得**被当作已发布数据）；
3. 复制游标回绕到 DR 登记册的 `last_confirmed_seq`，状态 `pending_reconcile`；
4. 本地日志/本地记账**不截断**——新主节点继续接受现场写入；
5. 续传从 `last_confirmed_seq + 1` 开始，因 upsert 幂等，不会重复计入。

### 1.6 审批撤回后交付必须重新核验

`POST /aggregates/{run}/deliver` 在**发送前**重新读取当前审批（状态/版本/有效期/区域范围/指标范围）与当前驻留政策，
并对**将要发送的确切字节**做结构扫描（禁止 `lat/lon/points/trajectory/...`）。任一检查失败：

- 交付状态 `blocked`，`artifact=null`、`artifact_sha256=""`（**不返回任何聚合内容**）；
- 写入 `delivery.blocked` 审计（`decision=deny`，含失败检查项与原因）；
- 同一聚合运行不能二次交付（`aggregate_run` 已达 `delivered` 时返回 `409`）。

### 1.7 审计链可证明"某份跨境报表从未包含受限原始轨迹"
- 出境文件按**原始字节**存 `text`（不是 `jsonb`，避免键重排/数字归一化破坏哈希），交付时记录 SHA-256；
- `GET /deliveries/{id}/verify` 重新计算哈希、扫描受限字段、校验对应审计条目与整条哈希链；
- 审计账本为哈希链（`prev_hash` + 规范化 payload），落库前对 `detail` 做 JSON 归一化、时间截断到微秒，
  保证"写入时哈希 == 读回后重算哈希"；任何越权改库都会被 `GET /audit/verify` 定位到具体行号。

### 1.8 作物生育阶段对齐（跨半球按共同阶段比较）

"第 18 周"在北半球与南半球并不处于同一生育阶段。系统新增**阶段对齐数据产品**：区域内保存作物、品种、
播种事实、阶段日历、温度观测及其覆盖；按同一季节规则版本推导阶段；集团只取得**区域粒度**的已播面积、
各阶段面积、待定面积和阶段证据范围——**不含田块标识或轨迹**。

- **同一季节规则版本绑定**：`rule_id`（含 `season_id` / `rule_version`）贯穿播种事实、品种目录、气象/日历证据、
  阶段推导、区域聚合与集团比较。混合季节规则的比较会被拒绝或列为 `mixed_season_rule`。
- **两条透明路线**，都由区域在本地计算：
  - `das`：已确认播种日 + 品种声明的**播后天数阈值**（无声明时回退到农艺部门发布的阶段日历）。规则透明，
    对没有连续气象资料的区域可用；
  - `gdd`：区域内温度站观测累计**生长度日**（基温 + 品种阈值）。更贴近实际物候，但要求温度站覆盖与基温/品种
    参数可信；缺样会得到**有界**的上界估计，并据此**扩大**证据范围。
- **不确定即待定，强行归类被禁止**：品种不明（`variety_unknown`）、补种或同地块多作物（`multi_crop_plot`）、
  温度站距离/高度不适用（`station_distance_or_height_not_applicable`）、缺站链接、缺气象、缺 GDD 参数、
  阶段证据互相矛盾（`stage_evidence_conflict`）、或缺样使阶段跨越的扩大范围超过阈值（`weather_gap_widens_stage`）
  时，面积列入 `pending`，绝不并入阶段面积。同一地块的补种按**分段**处理（`segments` 记录每个播次）。
- **不可变运行**：每次推导签发一个 `phenology_run`，记录其所读播种事实/气象观测的 `evidence_fingerprint`。
  迟到播种或气象修订只产生**新运行**，历史运行的阶段与证据范围原样保留。
- **集团比较只报告农艺进度**：`/phenology/comparisons` 只用已确认的区域阶段产品，输出共同阶段
  （`common_stage`）与**共同可比区域**（`common_comparable_regions`），并带明确声明：
  阶段对齐是农艺进度口径，**不评价团队绩效、不预测产量**。`region_as_of` 允许各区域按本地等价日期对齐
  （例如南半球 10 月播种 vs 北半球 4 月播种），而不是把公历周直接并列。

---

## 2. 快速启动

### 2.1 Docker Compose（一键）

```bash
docker compose up --build
# API:        http://localhost:8080
# Swagger UI: http://localhost:8080/docs
# OpenAPI:    http://localhost:8080/openapi.json
```

Compose 起 `postgres:18-alpine`（`ccdev/ccdev`，映射 5432）与 API 容器；API 启动时自动建库、建表、写入区域/政策/DR 登记册。
镜像地址可通过 `POSTGRES_IMAGE`、`GO_IMAGE`、`RUNTIME_IMAGE` 覆盖以适配镜像代理：

```bash
GO_IMAGE=<mirror>/golang:1.26-bookworm RUNTIME_IMAGE=<mirror>/distroless-static:nonroot docker compose up --build
```

### 2.2 本地开发（容器内已预装运行时）

```bash
# 依赖：PostgreSQL 18 已在 127.0.0.1:5432（库/用户/密码均为 ccdev）
/opt/postgresql-18/bin/pg_isready -h 127.0.0.1 -p 5432
export PATH=/opt/go/bin:$PATH
go build ./...
go run ./cmd/server            # 默认 :8080

# 可选：后台每 5s 自动复制（默认关闭，保证回放脚本确定性）
REPLICATION_INTERVAL=5s go run ./cmd/server
```

无 Docker 时同样可用 `make run`。数据库账号仅用于隔离开发/测试，不作为生产配置。

### 2.3 一键验证

```bash
make verify            # go build + vet + 单元测试 + 起临时服务 + 146 项验收断言
# 或对已运行实例：
./scripts/replay.sh http://localhost:8080
```

`scripts/replay.sh` 输出逐条 `PASS/FAIL`，失败时退出码非 0。`scripts/demo.sh` 是给人读的分阶段演示（不做断言）。

回放脚本有少量断言需要直接查库（去重后的面积、副本行数、越权改库），默认使用
`/opt/postgresql-18/bin/psql` 连接 `127.0.0.1:5432`（容器内预装）；在其他环境运行时可覆盖：
`PSQL=/usr/bin/psql PGHOST=... PGPORT=... PGUSER=... PGPASSWORD=... ./scripts/replay.sh http://localhost:8080`。
在 Docker Compose 场景下把 `PGHOST` 指向映射出来的 5432 即可（`docker compose up` 已映射）。

---

## 3. 测试账号（Bearer Token）

除 `/healthz`、`/openapi.json`、`/docs`、`/swagger-ui/*` 外，所有接口都需要 `Authorization: Bearer <token>`。

| 角色 | Token | 权限 |
| --- | --- | --- |
| 平台管理员 `admin` | `demo-admin-token` | 链接开关、批量复制、主备切换、重置演示数据 |
| 集团分析 `group_analyst` | `demo-analyst-token` | 提交出境申请、计算聚合、交付；**无**区域原始记录读取通道 |
| 数据管理员 `approver` | `demo-approver-token` | 审批 / 驳回 / 撤回；**不能**自行提交申请或交付 |
| 审计员 `auditor` | `demo-auditor-token` | 只读：审计、校验 |
| 区域操作 `region_operator` | `demo-operator-de-bb` | 仅 DE-BB 的写入与本地查询 |
| 区域操作 | `demo-operator-br-mt` | 仅 BR-MT |
| 区域操作 | `demo-operator-us-ia` | 仅 US-IA |
| 区域操作 | `demo-operator-fr-nor` | 仅 FR-NOR |

Swagger UI 中点击 **Authorize** 填入上述任一 token 即可调用。

---

## 4. 样例数据（`samples/`）

| 文件 | 用途 |
| --- | --- |
| `regions/de-bb.json` 等 4 个 | 每区域首批 4 个作业片段（含轨迹与告警） |
| `offline/br-mt-offline-backfill.json` | BR-MT 断网期间继续作业的 3 个片段（本地提交，链路恢复后补传） |
| `offline/br-mt-retry-overlap.json` | 断网补传时**重叠重传**：3 个已接收片段 + 1 个新片段（验证去重与面积不重复） |
| `dr/us-ia-post-switch.json` | 主备切换后在新主节点继续上传 |
| `approvals/wide.json` | 四区域 × 全指标的出境申请 |
| `approvals/narrow.json` | 三区域（DE-BB/BR-MT/US-IA）× 3 指标的窄范围申请 |
| `phenology/{de-bb,us-ia,br-mt,fr-nor}/evidence.json` | 每区域阶段证据：季节规则、品种（日数阈值 + GDD 参数）、地块、播种事实、阶段日历、温度站/链接、温度观测 |
| `phenology/de-bb/correction-v2.json` | 迟到更正：某地块播种日更高 revision + 一条气象观测修订（只影响**新**运行） |
| `phenology/approvals/phenology-wide.json` | 四区域、授权 `area_ha` 的阶段比较出境申请 |

重新生成：`python3 scripts/gen_samples.py && python3 scripts/gen_phenology_samples.py`（或 `make samples`）。

---

## 5. 验收回放（`scripts/replay.sh`）

脚本对运行中的服务做**可断言**的回放，覆盖题目要求的四类验收场景与审计链：

| 场景 | 断言示例 |
| --- | --- |
| 区域断网 | 断网后本地仍 `accepted=3`、日志 `entries=head=7` 且 `gapless=true`、`pending_outbox=7`；集团聚合 `excluded_regions/stale_regions = BR-MT`，合计只含其余三区 |
| 复制重试 | 重叠重传 `duplicates=3`；失败注入时游标 `confirmed_seq` 不动、`retry_count` 递增；重试成功后集团副本面积/行数 **等于** 区域自身面积/行数 |
| 主备切换 | 注入确认失败→存在 4 行 `unconfirmed`；failover 丢弃这 4 行、`resumed_from_seq == DR last_confirmed_seq`、新主 `epoch=2`、本地日志仍 `gapless`、续传后面积无重复；另注入一行崩溃写入者的孤儿副本，切换后同样被清除且计入报告 |
| 审批撤回 | 撤回后交付 `status=blocked`、`artifact=null`、无 `artifact_sha256`、原因为 `approval_status`；窄范围申请越权指标在聚合阶段 `403` |
| 出境审计 | 存储字节重算哈希一致、`restricted_fields` 为空、审计条目与整链校验通过；**越权改库后** `audit/verify` 返回 `valid=false` 且 `broken_at` 指向被改行 |
| 阶段对齐 | 同一 30 播后天数下，凉爽 DE-BB 按 GDD 仅到出苗、温暖 US-IA 到营养生长（`vegetative=130`），而 `das` 路线两者一致；补种地块 `segmented_plots=1`、`reseeded_area_ha=100`；BR-MT 缺样 → `weather_gap_widens_stage` 待定且证据范围变宽、`weather_coverage<0.7`；US-IA 站距 95km/高差 260m → 站不适用待定；FR-NOR 无日数阈值 → `calendar_used=true`、田块观测与推导矛盾 → `stage_evidence_conflict` 待定；迟到更正产生**新运行且指纹不同**、旧运行指纹不变；气象修订使下一 GDD 运行指纹变化而历史 GDD 运行不变；集团比较四区对齐、`common_stage=emergence`、交付无坐标、补种/多作物面积计入 `pending_totals`；审批撤回后阶段比较交付 `blocked` |
| 权限边界 | 无/错 token 401；区域操作员跨区 403；分析员自批 403；操作员触发全局复制/切换 403；集团分析读取区域原始记录 403；审计员可读区域记录用于审计 |

一次完整运行的实际结果（`make verify`，2026-11-19 在本容器内执行）：

```
checks passed: 146, failed: 0
ALL CHECKS PASSED
```

---

## 6. API 一览

完整定义见 `GET /openapi.json`（OpenAPI 3.0.3，42 路径），交互文档见 `/docs`。

| 分组 | 接口 |
| --- | --- |
| 区域 | `POST /regions/{region}/ingest`、`GET /regions/{region}/status`、`GET /regions/{region}/operations`、`GET /regions/{region}/journal/verify`、`GET /regions` |
| 复制 | `POST /replication/run`、`POST /replication/batch`、`GET /replication/cursors`、`GET /replication/runs`、`GET/POST /replication/links[/{region}]`、`GET /replication/policies`、`GET /replication/replicas` |
| 出境申请 | `GET/POST /outbound/requests`、`GET /outbound/requests/{id}`、`POST .../decide`、`POST .../revoke` |
| 聚合 | `GET/POST /aggregates`、`GET /aggregates/{id}` |
| 交付 | `POST /aggregates/{id}/deliver`、`GET /deliveries/{id}`、`GET /deliveries/{id}/verify` |
| 阶段对齐（区域） | `GET/POST /regions/{region}/phenology/evidence`、`GET/POST /regions/{region}/phenology/runs`、`GET /regions/{region}/phenology/runs/{run}` |
| 阶段对齐（集团） | `GET/POST /phenology/comparisons`、`GET /phenology/comparisons/{id}`、`POST /phenology/comparisons/{id}/deliver`、`GET /phenology/deliveries/{id}`、`GET /phenology/deliveries/{id}/verify` |
| 容灾 | `GET /dr`、`GET /dr/{region}`、`POST /dr/{region}/failover`、`POST /dr/{region}/failback` |
| 审计 | `GET /audit`、`GET /audit/verify`、`GET /audit/{subject}` |
| 演示 | `POST /demo/reset`（`DEMO_MODE=false` 时禁用）、`GET /demo/summary` |

复制故障注入（仅用于验收演练）：`{"fault":"link_down"|"copy_fail"|"confirm_fail"}`。

---

## 7. 手工走一遍（curl）

```bash
A='Authorization: Bearer demo-admin-token'
curl -s -XPOST localhost:8080/demo/reset -H "$A"
curl -s -XPOST localhost:8080/regions/BR-MT/ingest \
  -H 'Authorization: Bearer demo-operator-br-mt' -H 'Content-Type: application/json' \
  --data-binary @samples/regions/br-mt.json
curl -s -XPOST localhost:8080/replication/batch -H "$A" -H 'Content-Type: application/json' -d '{}'
curl -s localhost:8080/replication/replicas -H "$A"        # 只有 US-IA 有 raw_trajectories
curl -s localhost:8080/replication/policies -H "$A"        # 每个区域每类别的出境政策
```

出境链路（申请 → 审批 → 聚合 → 交付 → 验证）：

```bash
AN='Authorization: Bearer demo-analyst-token'; AP='Authorization: Bearer demo-approver-token'
APR=$(curl -s -XPOST localhost:8080/outbound/requests -H "$AN" -H 'Content-Type: application/json' \
  --data-binary @samples/approvals/wide.json | jq -r .id)
curl -s -XPOST localhost:8080/outbound/requests/$APR/decide -H "$AP" -H 'Content-Type: application/json' \
  -d '{"decision":"approved"}' | jq -c '{status,version}'
RUN=$(curl -s -XPOST localhost:8080/aggregates -H "$AN" -H 'Content-Type: application/json' \
  -d "{\"approval_id\":\"$APR\",\"period_from\":\"2026-11-01T00:00:00Z\",\"period_to\":\"2026-11-30T00:00:00Z\",\"metric_keys\":[\"operation_count\",\"completion_rate\",\"area_ha\"]}" | jq -r .id)
DLV=$(curl -s -XPOST localhost:8080/aggregates/$RUN/deliver -H "$AN" | tee /tmp/d.json | jq -r .id)
jq -c '{status,included:.artifact.included_regions,excluded:.artifact.excluded_regions,sha:.artifact_sha256}' /tmp/d.json
curl -s localhost:8080/deliveries/$DLV/verify -H 'Authorization: Bearer demo-auditor-token' | jq -c .
curl -s localhost:8080/audit/verify -H 'Authorization: Bearer demo-auditor-token' | jq -c .
```

阶段对齐链路（区域证据 → 两种路线推导 → 复制 → 跨半球比较 → 交付 → 验证）：

```bash
PH=samples/phenology
for r in de-bb us-ia br-mt fr-nor; do
  code=$(echo "$r" | tr a-z A-Z)
  curl -s -XPOST localhost:8080/regions/$code/phenology/evidence \
    -H "Authorization: Bearer demo-operator-$r" -H 'Content-Type: application/json' \
    --data-binary @$PH/$r/evidence.json | jq -c '{region,rule_id,counts}'
done

# 同一 30 播后天数：凉爽 DE-BB 与温暖 US-IA 在 DAS 下一致，在 GDD 下分化
for spec in "DE-BB de-bb das 2026-05-10" "DE-BB de-bb gdd 2026-05-10" \
            "US-IA us-ia das 2026-05-15" "US-IA us-ia gdd 2026-05-15"; do
  set -- $spec
  curl -s -XPOST localhost:8080/regions/$1/phenology/runs -H "Authorization: Bearer demo-operator-$2" \
    -H 'Content-Type: application/json' \
    -d "{\"rule_id\":\"S2026-MAIZE-v1\",\"method\":\"$3\",\"as_of_date\":\"$4\"}" \
    | jq -c '{method,stages:.summary.stage_areas,pending:.summary.pending_area_ha}'
done
# BR-MT 缺样 -> weather_gap_widens_stage 待定、证据范围变宽；FR-NOR 走日历回退
curl -s -XPOST localhost:8080/regions/BR-MT/phenology/runs -H 'Authorization: Bearer demo-operator-br-mt' \
  -H 'Content-Type: application/json' -d '{"rule_id":"S2026-MAIZE-v1","method":"gdd","as_of_date":"2026-11-05"}' \
  | jq -c '{stages:.summary.stage_areas,pending:.summary.pending,pending_evidence:.summary.pending_evidence,coverage:.summary.weather_coverage,reseeded:.summary.reseeded_area_ha}'

# 每个区域补一个比较日的运行，然后复制、比较、交付、验证
for spec in "DE-BB de-bb 2026-05-20" "US-IA us-ia 2026-05-20" "FR-NOR fr-nor 2026-05-20" "BR-MT br-mt 2026-11-05"; do
  set -- $spec
  curl -s -XPOST localhost:8080/regions/$1/phenology/runs -H "Authorization: Bearer demo-operator-$2" \
    -H 'Content-Type: application/json' -d "{\"rule_id\":\"S2026-MAIZE-v1\",\"method\":\"das\",\"as_of_date\":\"$3\"}" >/dev/null
done
curl -s -XPOST localhost:8080/replication/batch -H "$A" -H 'Content-Type: application/json' -d '{}' >/dev/null

AN='Authorization: Bearer demo-analyst-token'; AP='Authorization: Bearer demo-approver-token'; AU='Authorization: Bearer demo-auditor-token'
APR=$(curl -s -XPOST localhost:8080/outbound/requests -H "$AN" -H 'Content-Type: application/json' \
  --data-binary @samples/phenology/approvals/phenology-wide.json | jq -r .id)
curl -s -XPOST localhost:8080/outbound/requests/$APR/decide -H "$AP" -H 'Content-Type: application/json' -d '{"decision":"approved"}' >/dev/null
CMP=$(curl -s -XPOST localhost:8080/phenology/comparisons -H "$AN" -H 'Content-Type: application/json' \
  -d "{\"approval_id\":\"$APR\",\"rule_id\":\"S2026-MAIZE-v1\",\"method\":\"das\",\"as_of_date\":\"2026-05-20\",\"region_as_of\":{\"BR-MT\":\"2026-11-05\"}}")
echo "$CMP" | jq -c '{included:.included_regions,common:.common_stage,comparable:.common_comparable_regions,stage_totals,pending_totals}'
CID=$(echo "$CMP" | jq -r .id)
DLV=$(curl -s -XPOST localhost:8080/phenology/comparisons/$CID/deliver -H "$AN")
echo "$DLV" | jq -c '{status,coords:.artifact.contains_raw_trajectories,pending:.artifact.pending_totals}'
curl -s localhost:8080/phenology/deliveries/$(echo "$DLV" | jq -r .id)/verify -H "$AU" | jq -c .
```

---

## 8. 配置项

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `HTTP_ADDR` | `:8080` | 监听地址 |
| `PGHOST` / `PGPORT` / `PGUSER` / `PGPASSWORD` / `PGSSLMODE` | `127.0.0.1` / `5432` / `ccdev` / `ccdev` / `disable` | PostgreSQL 连接 |
| `PG_ADMIN_URL` | 空 | 建库用连接串（默认用 `PG_ADMIN_DB=postgres` 推导） |
| `CONTROL_DB` | `agrisync_control` | 控制面库名 |
| `REGION_DB_PREFIX` | `agrisync_region_` | 区域库前缀（后接小写化区域码） |
| `REGIONS` | `DE-BB,BR-MT,US-IA,FR-NOR` | 启用的区域 |
| `REPLICATION_BATCH` | `500` | 单次复制最大事件数 |
| `REPLICATION_INTERVAL` | 空（关闭） | 后台复制间隔，如 `5s`；关闭时复制完全由 API 驱动 |
| `DELIVERY_CHANNEL` | `secure_store` | 交付通道标签（写入交付记录与审计） |
| `DEMO_MODE` | `true` | `false` 时禁用 `/demo/reset` |
| `TOKEN_*` | 见第 3 节 | 各角色令牌，生产环境必须覆盖 |

---

## 9. 目录结构

```
cmd/server/            服务入口（含 -healthcheck 子命令，供 distroless 健康检查）
internal/api/          HTTP 路由、鉴权、OpenAPI 文档、后台复制器
internal/service/      区域写入/去重、复制、聚合、审批与交付、容灾、审计链、生育阶段推导与比较
internal/db/           PostgreSQL 连接池（每库一个）、schema、种子、重置
internal/config/       区域/令牌/连接串配置
internal/model/        领域模型（phenology.go 为阶段对齐）
web/swagger-ui/        内置 Swagger UI 资产（离线可用，Apache-2.0，见 LICENSE/NOTICE）
samples/               多区域、断网补传、重试重叠、切换后续传、不同授权范围、阶段对齐证据
samples/phenology/     各区域季节证据、迟到更正样本、阶段比较审批
scripts/               gen_samples.py、gen_phenology_samples.py、replay.sh（断言）、demo.sh（演示）、verify.sh
docs/architecture.md   数据流与失败模式说明
```

## 10. 已知边界

- 交付通道 `secure_store` 是本地归档语义（文件存于 `deliveries.artifact`），不包含真实外部传输；接入真实接收方时在
  `Deliver` 的 `sent` 分支替换即可，审计与复核逻辑不变。
- 驻留政策目前通过种子数据维护（只读 API 暴露），政策变更是治理动作，不在演示界面内修改。
- DR 是登记册 + 本地主备角色指针的模拟：单进程内切换，不包含真实流复制；但"只从已确认位置续传"的约束是真实执行的。
- 后台复制默认关闭以确保回放确定性；开启后与回放脚本并行会有竞态（脚本会自行重新复制并断言，通常仍能通过，但不建议）。
- 阶段对齐是**农艺进度口径**：不做产量预测、不评价团队绩效、不对天气/土壤/管理差异做校正。共同可比区域仅表示
  "均已到达共同阶段且面积未被判为待定"。
- GDD 的缺样上界以观测到的日最大累积率 × `missing_day_upper_fraction` 作有界外推，仅用于**扩大证据范围**，绝不回填观测；
  缺样导致阶段跨越超过 `weather_gap_pending_days` 时面积判为待定。基温/品种阈值来自品种目录，系统不自行估计。
- 阶段比较在生成时解析各区域"该规则/方法/日期下最新已确认运行"；如需锁定历史依据，用请求体 `region_runs` 指定具体运行，
  或用 `region_as_of` 为不同半球指定本地等价日期。
