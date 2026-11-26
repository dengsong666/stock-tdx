# HTTP 服务

生产进程通过 `routes.NewRootHandler` 统一装配 HTTP 路由。每项业务服务独占一个子目录，根包只保留健康检查、Web Viewer 挂载、CORS 和服务装配。

```text
routes/
├── root.go
├── unusual/  # 批量个股异动 SSE
├── quotes/   # TDX 批量股票行情
├── kline/    # 单股票时间区间 K 线
├── auction/  # 批量股票多日竞价金额
└── opening/  # 批量股票某日开盘成交（09:25 竞价成交与 09:30 后第一笔）
```

当前地址：

- `/`：跳转到 `/web`
- `/web`：Web Viewer 页面
- `/web/api/methods`、`/web/api/query`：Web Viewer 调试接口
- `/api/health`：HTTP 服务存活检查
- `/api/stock/unusual/sse`：批量个股异动 SSE
- `/api/stock/quotes`：TDX 批量股票行情
- `/api/stock/kline`：单股票时间区间 K 线
- `/api/stock/auction-amounts`：批量股票多日竞价金额
- `/api/stock/opening-trades`：批量股票某日开盘成交

以上地址（`/api/health` 除外）都需要带共享 JWT，浏览器侧推荐经主站 `/api/proxy/tdx/**` 访问，详见“CORS 与鉴权”。

## 批量股票某日开盘成交

```http
POST /api/stock/opening-trades
Content-Type: application/json

{"date":"20210512","codes":["001202","600519"]}
```

`date` 为 `YYYYMMDD` 交易日；单次最多 20 只沪深北 A 股，重复代码按首次出现顺序去重。

成功响应：

```json
{
  "code": 0,
  "msg": "success",
  "data": {
    "trades": {
      "001202": {
        "auction_time": "09:25:03",
        "auction_price": 25.93,
        "first_time": "09:30:00",
        "first_price": 26.02,
        "first_vol": 674
      },
      "600519": {
        "auction_time": "09:25:04",
        "auction_price": 1949.08,
        "first_time": "09:30:00",
        "first_price": 1952.59,
        "first_vol": 9
      }
    },
    "missing_codes": []
  }
}
```

（上面是 2026-11-25 实测返回。）

读 MAC 历史分笔（约 3 秒一条快照、带秒，回溯至少到 2020-07）：`auction_*` 是当天 09:25 那条集合竞价成交，没有时为 `null`；
`first_*` 是 09:30:00 及以后第一条快照（快照内最后成交价），价格单位元、按分四舍五入，`first_vol` 单位手；价格为 0 的占位记录跳过。
当天没有 09:30 之后成交的代码（停牌、日期未开盘或非交易日）列入 `missing_codes`。

MAC 分笔从收盘往前分页，历史日要翻完整天（约 5 页）才拿得到开盘那头；盘中 09:31 前后查当天只有一页。
每次 HTTP 请求复用一条 MAC 短连接；某只股票查询失败时断开并换下一个主站**新建**连接重试（gotdx 的 `Disconnect` 会把客户端永久标记为关闭，
不能复用），全部主站都失败才整批返回 502。供 adata 采集 `bid_strong.open_first_price`（模型的第一笔价买入口径）。

## 批量股票多日竞价金额

```http
POST /api/stock/auction-amounts
Content-Type: application/json

{"codes":["600127","000001"],"days":3}
```

`days` 表示最近多少个有集合竞价成交的交易日，范围为 1 到 10；单次最多查询 20 只沪深北 A 股，重复代码按首次出现顺序去重。

成功响应：

```json
{
  "code": 0,
  "msg": "success",
  "data": {
    "auctions": {
      "600127": [
        {"date": "20260826", "amount_bid": 11193, "amount_prev": 80000},
        {"date": "20260825", "amount_bid": 8815, "amount_prev": 70000},
        {"date": "20260824", "amount_bid": 8293, "amount_prev": 65000}
      ]
    },
    "missing_codes": []
  }
}
```

返回结果已经按 `code` 分组在 `data.auctions` 下，`date` 使用 `YYYYMMDD` 格式，`amount_bid` 和 `amount_prev` 的单位都是万元，并按四舍五入返回整数。服务按 20 根一页查询原始一分钟 K 线，用每天的 `09:31` 记录定位交易日期，再读取当天历史逐笔成交；只有找到 `09:25` 集合竞价成交时才按 `成交价 × 成交量（手）× 100 ÷ 10000` 计算 `amount_bid`，并从日线中查找该日期前一个交易日的全天成交额计算 `amount_prev`。找不到前一个交易日成交额时，整条记录不返回；完全没有有效记录的代码返回空数组并列入 `missing_codes`。

每次 HTTP 请求使用一个独立的传统主行情短连接，多个股票顺序查询，完成后断开；使用 gotdx 已有公开 API，不修改客户端或协议实现。

## 股票与指数时间区间 K 线

```http
POST /api/stock/kline
Content-Type: application/json

{
  "type": "stock",
  "code": "600519",
  "period": "5m",
  "start": "2026-07-01 09:30:00",
  "end": "2026-07-24 15:00:00",
  "adjust": "qfq"
}
```

`start` 必填；`end` 可选，省略时使用请求时的北京时间。时间支持 `YYYY-MM-DD` 或 `YYYY-MM-DD HH:mm:ss`，起止边界均包含。日期型 `end` 覆盖到当天结束。

`type` 支持 `stock`、`index`，省略时默认 `stock`。股票只接受沪深北 A 股六位代码，由服务内部推断市场；指数还必须传 `market=sh|sz|bj`，例如 `{"type":"index","market":"sh","code":"000001",...}`。

周期支持 `1m`、`3m`、`5m`、`15m`、`30m`、`60m`、`day`、`week`、`month`、`year`，不提供 `120m`。股票复权支持 `none`、`qfq`、`hfq`，省略时默认 `qfq`；指数固定为 `none`。股票 3 分钟使用 TDX 的“1 分钟周期 × 3”，指数 3 分钟走传统主站时由服务按交易时段聚合指数 1 分钟数据，走扩展行情时直接由协议的倍率参数产出。

成功响应：

```json
{
  "code": 0,
  "msg": "success",
  "data": [
    {
      "time": "2026-07-24 14:55:00",
      "open": 1410,
      "high": 1418.5,
      "low": 1408,
      "close": 1416,
      "volume": 123456,
      "amount": 174800000,
      "turnover": 0.12,
      "prev_close": 1409,
      "change": 7,
      "change_pct": 0.5
    }
  ]
}
```

`data` 直接返回按时间正序排列的 K 线数组，不重复请求元数据，也不计算 `is_complete`。数值保留 gotdx 从 TDX 解码出的精度。指数没有换手率时 `turnover` 返回 `0`。单次最多返回 20,000 根，超过限制返回 HTTP 400，调用方应缩小时间范围。

股票通过 MAC 主站调用 gotdx 已有 `MACSymbolBars`。指数有两条链路：先走传统主行情服务器的 `GetIndexBars`，返回空时自动兜底到扩展行情服务器的 `ExGetKLine`，因为中证系指数（如中证全指 000985）只挂在扩展行情。扩展行情按分类号 62 取数，成交额以百万元取整、成交量在分钟周期以万手、日线及以上以百万手取整，服务换算成与东财、传统主站一致的元与手，因此小指数的大周期成交量会有约 1% 的取整误差。两条链路都用历史偏移搜索和分页实现时间范围查询；服务端单页最多返回 699 根（传统主行情与扩展行情一致），因此页大小固定为 699，短页不视为历史尽头，会继续按偏移翻页。都为空时返回 `data: []` 而不是错误。每次 HTTP 请求使用独立短连接，单主站超时 3 秒，失败后切换同类型的下一主站；不修改 SDK 的客户端或协议实现。

## TDX 批量股票行情

```http
POST /api/stock/quotes
Content-Type: application/json

{"codes":["600519","300750","000001"],"indexes":[{"market":"sh","code":"000001"}]}
```

`indexes` 可选，用于指数行情：指数与股票存在同号代码（`000001` 既是平安银行也是上证指数），必须显式带市场（`sh`/`sz`/`bj`），响应键拼成 `sh000001`，与股票代码互不覆盖。`codes` 与 `indexes` 至少填一个，合计最多 1000 个；同一代码不能同时出现在多个市场。

成功响应统一使用 `code`、`msg`、`data`：

```json
{
  "code": 0,
  "msg": "success",
  "data": {
    "quotes": {
      "600519": {
        "code": "600519",
        "price": 1297.41,
        "chg": 0.42,
        "high": 1309.21,
        "low": 1286.2,
        "open": 1305,
        "prev_close": 1292.01,
        "limit_up_price": 1421.21,
        "updated_at": "2026-07-24 15:30:57"
      }
    },
    "index_quotes": {},
    "missing_codes": [],
    "missing_indexes": []
  }
}
```

错误响应的 `data` 为 `null`，`code` 与 HTTP 状态码一致。请求最多包含 1000 个六位证券代码，重复代码按首次出现位置去重。服务自动识别沪市、深市和北交所，但不在响应中暴露市场字段。

每次 HTTP 请求使用独立 TDX MAC 短连接，从最近成功主站开始尝试。单个主站超时 3 秒，失败后切换下一主站；协议成功但未返回的股票通过 `missing_codes`、指数通过 `missing_indexes` 告知调用方。指数行情同样走 MAC 批量报价，只是入参带市场号；实测五条基准指数一次可取回。

## 批量个股异动 SSE

```text
GET /api/stock/unusual/sse?stocks=000001,600000&since=12345
```

服务把「采集」与「推送」拆成两层：

- 采集层在工作日 09:15–11:30、13:00–15:05 常驻运行，每 2 秒轮询深市、沪市、北交所的 `MACMarketMonitor`，把事件写入内存缓冲并分配单调递增游标 `seq`。**没人订阅也在采集**，因此手机端掉线期间的事件不会丢失。
- **缓冲按交易日保留，不按自然日清空**：周五收盘后到下一个交易日 09:15 之前，缓冲里始终是上一个交易日的数据，周末打开 app 仍能看到。切换发生在「工作日 09:15 之后第一次采集」，同时复位游标。
- **午休（11:30–13:00）不采集**：没有交易就没有新异动。游标停在 11:30 的位置，13:00 恢复后一次增量请求就能把午休期间延迟写入的事件全部补齐，不会漏。
- 推送层只在有订阅者时把新事件 fan-out 出去；`since` 落在缓冲范围内时先补发 `(since, 当前游标]` 的事件，实现断线补洞。

服务启动后首次采集对每个市场做一次当天全量回填（约 2 秒、两万余条），回填期间照常广播给已连接的订阅者；因此服务重启后当天已发生的事件仍在缓冲中，重启瞬间连上的客户端也不会缺数据。

### 回填时机

| 服务启动时机 | 行为 |
| --- | --- |
| 交易时段内（09:15–11:30、13:00–15:05） | 首个采集周期立即回填当天全量，之后按 2 秒增量 |
| 午休（11:30–13:00） | 回填一次当天全量（午休重启时缓冲为空，不补的话午休打开 app 看不到上午），之后停采到 13:00 |
| 工作日收盘后（15:05 之后） | 回填一次当天全量，之后不再轮询 |
| 周末、次日凌晨 | 有落盘的交易日标注时照常回填，并沿用上次标注（不猜今天几号）；没有标注（首次部署就赶上周末）则不回填，避免把数据标错日期 |

所以中午重启不会丢早上的事件：缓冲清空后立即按当天全量重新回填。

缓冲所属的交易日会落盘到 `data/unusual-buffer-date`（可用 `TDX_UNUSUAL_DATE_FILE` 覆盖路径），只存一个日期字符串、不存事件内容。作用是服务在非交易时段重启后沿用同一标注：上游那时返回的就是这一批数据，不需要判断今天几号。

### 游标与重启

`seq` 起点取服务启动时刻的毫秒时间戳，跨进程重启单调递增。客户端把上次收到的最大 `seq` 存下来作为重连的 `since`，重启后旧游标必然小于新缓冲起点，服务端返回 `reset`，客户端清空游标重新对齐。

**不要把起点写成 0 或 1**：那样重启后旧 `since` 会落在新游标空间的中间，服务端既不会发 `reset`，又会把新空间里的前若干条事件当成"客户端已读"而跳过，客户端静默漏数据且无从察觉。

### 事件帧

| 事件 | 时机 | data |
| --- | --- | --- |
| `ready` | 连接建立后立即发送 | `{"seq":N}`，客户端记下 N 作为下次重连的 `since`；随后会补发缓冲内的事件 |
| `unusual` | 每条新异动 | 见下方字段表 |
| `reset` | `since` 早于缓冲起点（跨日或缓冲被淘汰） | `{"baseSeq":N}`，客户端应清空本地缓存并从随后事件重新开始 |
| `error` | 采集连续失败，仅在不健康转态时发一次；客户端应把它当作「本源暂不可用」处理 | `{"message":"MAC 行情轮询失败"}` |

`unusual` 字段：

| 字段 | 说明 |
| --- | --- |
| `seq` | 全局单调递增游标，跨日不清零 |
| `date` | 该事件所属的交易日 `YYYY-MM-DD`。客户端必须用它作为日期标注，不能用打开当天的日期——周末收到的是上一个交易日的数据 |
| `code` / `name` | 证券代码与名称 |
| `time` | 异动时间 `HH:MM:SS` |
| `desc` | 中文类型，含 `封涨大减` / `封跌大减` / `逼近涨停` / `大单托盘` 等 33 类 |
| `value` | 类型相关数值；涨跌停类是 `价格/当时封单剩余量(手)` |
| `type` | 协议原始类型字节（`0x03`–`0x1e`），不是东方财富的编码 |
| `raw` | 协议原始附属字段，`0x14`（涨跌停组）下存在偏移错位，**不要用于计算** |

直接注册子服务：

```go
package main

import (
	"net/http"

	gotdx "github.com/bensema/gotdx"
	"github.com/bensema/gotdx/routes/unusual"
)

func main() {
	client := gotdx.NewMAC()
	mux := http.NewServeMux()
	// 注册即启动常驻采集；Close 可停止采集并断开订阅者。
	service := unusual.RegisterStockUnusualSSE(mux, client)
	defer service.Close()
	_ = http.ListenAndServe(":8080", mux)
}
```

参数规则：

- `stocks` 必填，使用英文逗号分隔；代码必须是可识别市场的六位 A 股代码。
- 重复代码自动去重，单个连接最多订阅 500 只股票。
- `since` 可选，非负整数游标。**缺省表示补发缓冲内的当天全量**（app 可能下午或收盘后才第一次打开，必须能看到当天已发生的异动）；带 `since` 时只补发它之后的增量。
- 采集覆盖深市、沪市、北交所，与订阅了哪些股票无关。
- 连续三次查询失败后发送 `error` 事件，连接保持打开并继续恢复。
- 订阅通道被写满时主动断开该连接，由客户端带 `since` 重连补齐。
- 每分钟发送一次 SSE 注释心跳。

## CORS 与鉴权

所有业务 API、SSE 和 Web Viewer 都校验共享 JWT，只有健康检查 `/api/health` 和模型内部任务令牌例外；缺少有效令牌时统一返回 `401` 和 `{"error":"请先登录"}`。鉴权在 `routes/root.go` 的 `jwtAuth` 中统一完成，校验签名、`purpose=access` 和过期时间，密钥取 `JWT_SECRET`。

根路由只允许 `dengsong.online` 及其子域的浏览器跨域请求，且 CORS 允许的请求头只有 `Content-Type`，因此浏览器无法直接跨域携带 `Authorization` 调用 TDX。

### 主站代理

主站 Web 服务提供 `/api/proxy/tdx/**`，用登录用户的访问令牌把请求转发到 `TDX_API_BASE_URL`（容器内默认 `http://tdx:8883`），路径原样透传：

| 主站地址 | 转发到 TDX |
| --- | --- |
| `GET /api/proxy/tdx/web` | `GET /web`（Web Viewer 页面） |
| `POST /api/proxy/tdx/web/api/query` | `POST /web/api/query` |
| `POST /api/proxy/tdx/api/stock/quotes` | `POST /api/stock/quotes` |

浏览器只访问主站同源地址，认证走主站的 HttpOnly 会话 Cookie，不需要跨域，也不需要把 TDX 域名暴露给浏览器。Web Viewer 页面按当前访问路径推导调试接口地址，直接访问 `/web` 和经主站代理访问都能正常工作。

服务间调用使用 `INTERNAL_TASK_TOKEN`，例如 adata 通过 `http://tdx:8883` 调用行情接口。
