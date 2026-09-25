# materiallab — 风险批号抽样规划服务

从 1–5000 个风险批号闭区间中，抽取**最少**的实物批号，使每个风险区间命中其 `required_hits` 份**相互独立**的实物样本（同一批号在同一区间内只能充当一次样本，但可同时覆盖多个不同区间）；冻结批号段内禁止取样。纯 Go 1.23 后端，`net/http` 提供普通 JSON 接口，Docker Compose 运行。

## 问题与算法

- 输入：风险闭区间（`id` 唯一）与至多 5000 个可重叠的冻结闭区间，端点均为 `[0, 10^9]` 内整数且 `start ≤ end`。每个区间可带 `required_hits ∈ [1,3]`，未填写时保持原先“命中一次”的语义。
- **规范化冻结段**：按起点排序后合并重叠或**相接**（`[1,2]` 与 `[3,4]` 之间没有可选整数）的段，得到互不相交、升序且至少间隔一个自由整数的段列表。
- **处理顺序**：风险区间按 `end` 升序稳定排序；`end` 相同按 `id` 字典序，再相同保留输入顺序（`sort.SliceStable`）。
- **贪心选择**：按处理序逐个处理。先统计当前区间内已有的不同已选批号数（已选批号严格递增，区间内命中恰为有序列表的一个后缀）；不足 `required_hits` 时，**从右向左**选取尚未使用且不在冻结段内的最大整数批号（右端点在冻结段内就跳到该段起点的前一个整数），直到凑够份数。
  - 最右可行点贪心对“闭区间带需求的最小命中点”是最优解（交换论证）；跳过冻结段后仍保持最优——任何可行解在该区间内取的点都不大于所选点，用所选点替换不会丢失对后续区间的覆盖。
  - 这也避免了“先合并区间再随便挑点”会跳过唯一可用批号的问题。
- 若某个风险区间的可用整数不足 `required_hits` 个，立即返回 `NO_SAMPLE_POINT`，报告**按处理序最早**失败的区间（1 基位置）及**缺口数** `missing`，不返回任何部分方案。

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
    {"id": "R1", "start": 1, "end": 5, "required_hits": 2},
    {"id": "R2", "start": 6, "end": 10}
  ],
  "frozen": [{"start": 5, "end": 5}]
}
```

```json
{
  "status": "OK",
  "sample_count": 3,
  "points": [
    {"batch": 3,  "covered_ids": ["R1"]},
    {"batch": 4,  "covered_ids": ["R1"]},
    {"batch": 10, "covered_ids": ["R2"]}
  ],
  "interval_hits": [
    {"id": "R1", "batches": [3, 4]},
    {"id": "R2", "batches": [10]}
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
  "missing": 1
}
```

`points` 按批号升序；`covered_ids` 按处理序列出该点覆盖的区间，每个风险区间恰好被分配 `required_hits` 次。`interval_hits` 按处理序列出每个区间实际命中的批号（升序、互不相同）；`missing` 是失败区间的可用批号缺口数。

### 校验（HTTP 422）

以下任意情况整次返回 `422 {"error": "..."}`：

- 未知字段（顶层或嵌套，解码使用 `DisallowUnknownFields`）；
- 不是单个 JSON 对象、JSON 语法错误、含尾随内容；
- `intervals` 缺失或数量不在 1–5000；`frozen` 缺失或多于 5000；
- 区间缺 `id/start/end`、`id` 为空或重复；冻结段缺端点；
- `required_hits` 不是 `[1, 3]` 内的整数（缺省为 1）；
- 端点不是整数、超出 `[0, 10^9]`，或 `start > end`；
- 请求体超过 2 MiB。

## 测试

```bash
go test ./...
```

- `internal/planner`：在 0..4 小坐标上枚举**全部 32 种冻结掩码 × 全部单点/区间对/区间三元组 × 每区间 `required_hits ∈ {1,2,3}`**（62 万余个实例），与“枚举所有自由整数子集”的暴力最优解对拍（点数最少性、可行性、每区间命中份数与互异性、点/区间覆盖对应、批号严格递增、确定性、处理序、失败区间位置与缺口数）；另含 400 组随机实例（`required_hits ∈ {0..3}`）。
- 定点用例：重叠需求共享批号、相接冻结段合并（`[0,1]+[2,3]→[0,3]`，而 `[1,2]` 与 `[4,5]` 不合并）、单点区间（含 `required_hits=2` 不可行）、多个同右端点的稳定排序、冻结段把取样点向左推、部分选点后仍不足的确切缺口、10⁹ 边界值。
- `internal/api`：成功/失败响应（含 `interval_hits` 与 `missing`）、`required_hits` 合法性与全部 422 场景（含未知字段、重复 id、浮点/字符串端点、两个 JSON 值等）、5000 条上限。
