# materiallab — 风险批号抽样规划服务

从 1–5000 个风险批号闭区间中，抽取**最少**的实物批号，使每个风险区间至少命中 `requiredHits`（1～3，可省略，缺省 1）个**不同**批号；冻结批号段内禁止取样。纯 Go 1.23 后端，`net/http` 提供普通 JSON 接口，Docker Compose 运行。

## 问题与算法

- 输入：风险闭区间（`id` 唯一，可选 `requiredHits` 为 1～3 的独立实物份数）与至多 5000 个可重叠的冻结闭区间，端点均为 `[0, 10^9]` 内整数且 `start ≤ end`。
- 同一批号在同一区间内**不能重复计数**（命中按不同批号计算），但一个已选批号可以同时覆盖多个不同区间。
- **规范化冻结段**：按起点排序后合并重叠或**相接**（`[1,2]` 与 `[3,4]` 之间没有可选整数）的段，得到互不相交、升序且至少间隔一个自由整数的段列表。
- **处理顺序**：风险区间按 `end` 升序稳定排序；`end` 相同按 `id` 字典序，再相同保留输入顺序（`sort.SliceStable`）。
- **贪心选择**：按处理序逐个处理。先数当前区间**已有的不同已选批号**；不足 `requiredHits` 时，从右向左补选**尚未使用且不在冻结段内**的整数批号（若右端点在冻结段内，跳到该段起点的前一个整数），直到补齐。
  - 最右可行点贪心对“闭区间最小命中点（含每区间多份需求）”是最优解（交换论证：任何可行解在当前区间内取的点都不大于所选点，用所选点替换不会丢失对后续区间的覆盖；已处理区间的需求已由先前选出的点满足）；跳过冻结段与已用批号后仍保持最优。
  - 未填写 `requiredHits` 时按 1 处理，保持原先命中一次的语义。
- 若某个风险区间内可选的非冻结整数不足以满足 `requiredHits`，立即返回 `NO_SAMPLE_POINT`，并报告**按处理序最早**失败的区间（1 基位置）与**缺口数**（`requiredHits` 减去区间内可用整数数），不返回任何部分方案。

## 运行

```bash
docker compose up --build -d
curl -fsS http://localhost:8080/healthz
```

本地直接运行：`go run ./cmd/api`（默认监听 `:8080`，可用 `ADDR` 覆盖）。

## 接口

### `POST /sample-plan`

成功（HTTP 200）：

```json
{
  "intervals": [
    {"id": "R1", "start": 1, "end": 5, "requiredHits": 2},
    {"id": "R2", "start": 3, "end": 6, "requiredHits": 3}
  ],
  "frozen": [{"start": 5, "end": 5}]
}
```

```json
{
  "status": "OK",
  "sample_count": 3,
  "points": [
    {"batch": 3, "covered_ids": ["R1", "R2"]},
    {"batch": 4, "covered_ids": ["R1", "R2"]},
    {"batch": 6, "covered_ids": ["R2"]}
  ],
  "interval_hits": [
    {"interval_id": "R1", "batches": [3, 4]},
    {"interval_id": "R2", "batches": [3, 4, 6]}
  ],
  "processing_order": ["R1", "R2"]
}
```

无可用点（HTTP 200，整次规划作废）：

```json
{
  "status": "NO_SAMPLE_POINT",
  "sample_count": 0,
  "points": [],
  "processing_order": ["R1"],
  "failed_interval_id": "R1",
  "failed_position": 1,
  "failed_missing": 1
}
```

`points` 按批号升序；`covered_ids` 按处理序列出几何上包含该批号的区间。`interval_hits` 按处理序列出每个区间实际命中的不同批号（升序），数量不少于其 `requiredHits`。

### 校验（HTTP 422）

以下任意情况整次返回 `422 {"error": "..."}`：

- 未知字段（顶层或嵌套，解码使用 `DisallowUnknownFields`）；
- 不是单个 JSON 对象、JSON 语法错误、含尾随内容；
- `intervals` 缺失或数量不在 1–5000；`frozen` 缺失或多于 5000；
- 区间缺 `id/start/end`、`id` 为空或重复；冻结段缺端点；
- 端点不是整数、超出 `[0, 10^9]`，或 `start > end`；
- `requiredHits` 出现但不是 1～3 的整数（含小数、字符串）；
- 请求体超过 2 MiB。

## 测试

```bash
go test ./...
```

- `internal/planner`：在 0..4 小坐标上枚举**全部 32 种冻结掩码 × 全部单点/区间对/区间三元组 × requiredHits 组合**（单区间 1～3 全枚举、区间对 9 种需求组合、三元组均匀与混合需求，共约 16.7 万个实例），与“枚举所有自由整数子集”的暴力最优解对拍（点数、可行性、每区间不同命中数、命中/覆盖转置一致性、批号严格递增、确定性、处理序、失败区间位置与缺口数）；另含 400 组随机实例。
- 定点用例：重叠多需求共享批号、相接冻结段合并（`[0,1]+[2,3]→[0,3]`，而 `[1,2]` 与 `[4,5]` 不合并）、单批区间无法充当多份样本、单点区间、多个同右端点的稳定排序、冻结段把取样点向左推、10⁹ 边界值取三份样本。
- `internal/api`：成功/失败响应（含 `interval_hits` 与 `failed_missing`）与全部 422 场景（含未知字段、重复 id、浮点/字符串端点、非法 `requiredHits`、两个 JSON 值等）、5000 条上限。
