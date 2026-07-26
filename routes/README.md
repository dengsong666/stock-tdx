# HTTP 服务

生产进程通过 `routes.NewRootHandler` 统一装配 HTTP 路由。每项业务服务独占一个子目录，根包只保留健康检查、Web Viewer 挂载、CORS 和服务装配。

```text
routes/
├── root.go
├── unusual/  # 批量个股异动 SSE
├── quotes/   # TDX 批量股票行情
└── kline/    # 单股票时间区间 K 线
```

当前地址：

- `/`：跳转到 `/web`
- `/web`：Web Viewer 页面
- `/web/api/methods`、`/web/api/query`：Web Viewer 调试接口
- `/api/health`：HTTP 服务存活检查
- `/api/stock/unusual/sse`：批量个股异动 SSE
- `/api/stock/quotes`：TDX 批量股票行情
- `/api/stock/kline`：单股票时间区间 K 线

## 单股票时间区间 K 线

```http
POST /api/stock/kline
Content-Type: application/json

{
  "code": "600519",
  "period": "5m",
  "start": "2026-07-01 09:30:00",
  "end": "2026-07-24 15:00:00",
  "adjust": "qfq"
}
```

`start` 必填；`end` 可选，省略时使用请求时的北京时间。时间支持 `YYYY-MM-DD` 或 `YYYY-MM-DD HH:mm:ss`，起止边界均包含。日期型 `end` 覆盖到当天结束。

周期支持 `1m`、`3m`、`5m`、`15m`、`30m`、`60m`、`day`、`week`、`month`、`year`。其中 `3m` 使用 TDX 的“1 分钟周期 × 3”能力，不提供 `120m`。复权支持 `none`、`qfq`、`hfq`，省略时默认前复权 `qfq`。

接口只接受沪深北 A 股六位代码，由服务内部推断市场；不接受 ETF、可转债、B 股、指数、港股或美股代码，响应也不暴露市场字段。

成功响应：

```json
{
  "code": 0,
  "msg": "success",
  "data": {
    "code": "600519",
    "period": "5m",
    "start": "2026-07-01 09:30:00",
    "end": "2026-07-24 15:00:00",
    "adjust": "qfq",
    "klines": [
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
        "change_pct": 0.5,
        "is_complete": true
      }
    ]
  }
}
```

K 线按时间正序返回，数值保留 gotdx 从 TDX 解码出的精度。`is_complete` 表示对应周期是否已经结束；当前仍在形成的最后一根为 `false`。单次最多返回 20,000 根，超过限制返回 HTTP 400，调用方应缩小时间范围。

服务使用 gotdx 已有 `MACSymbolBars` 公开 API，通过历史偏移搜索和分页实现时间范围查询。每次 HTTP 请求使用独立短连接，单主站超时 3 秒，失败后切换下一主站；不修改 SDK 的客户端或协议实现。

## TDX 批量股票行情

```http
POST /api/stock/quotes
Content-Type: application/json

{"codes":["600519","300750","000001"]}
```

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
    "missing_codes": []
  }
}
```

错误响应的 `data` 为 `null`，`code` 与 HTTP 状态码一致。请求最多包含 1000 个六位证券代码，重复代码按首次出现位置去重。服务自动识别沪市、深市和北交所，但不在响应中暴露市场字段。

每次 HTTP 请求使用独立 TDX MAC 短连接，从最近成功主站开始尝试。单个主站超时 3 秒，失败后切换下一主站；协议成功但未返回的股票通过 `missing_codes` 告知调用方。

## 批量个股异动 SSE

```text
GET /api/stock/unusual/sse?stocks=000001,600000
```

它每 2 秒轮询一次通达信 `MACMarketMonitor`，按股票代码过滤新异动，再通过 SSE 逐条推送。第一次成功轮询只建立当天尾部基线，不补发连接前事件。

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
	unusual.RegisterStockUnusualSSE(mux, client)
	_ = http.ListenAndServe(":8080", mux)
}
```

参数规则：

- `stocks` 必填，使用英文逗号分隔。
- 股票代码必须是可识别市场的六位 A 股代码。
- 重复代码自动去重，单个连接最多订阅 100 只股票。
- 同一市场的 SSE 客户端共享轮询。
- 连续三次查询失败后发送 `error` 事件，连接保持打开并继续恢复。
- 每分钟发送一次 SSE 注释心跳。

## CORS 与鉴权

根路由只允许 `dengsong.online` 及其子域的浏览器跨域请求。业务服务不使用 Token 鉴权；部署层应根据实际用途决定是否公开对应域名。
