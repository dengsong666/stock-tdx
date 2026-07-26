// Package kline 提供面向业务调用的单股票 K 线 HTTP 服务。
package kline

import (
	"fmt"
	"strings"
	"time"

	"github.com/bensema/gotdx/types"
)

const dateTimeLayout = "2006-01-02 15:04:05"

var shanghaiLocation = time.FixedZone("Asia/Shanghai", 8*60*60)

type periodKind uint8

type assetType string

const (
	assetStock assetType = "stock"
	assetIndex assetType = "index"
)

const (
	periodMinute periodKind = iota
	periodDay
	periodWeek
	periodMonth
	periodYear
)

// Period 描述外部周期名称及其 TDX 协议参数。
type Period struct {
	Name          string
	Category      uint16
	IndexCategory uint16
	Times         uint16
	Kind          periodKind
	Duration      time.Duration
}

var supportedPeriods = map[string]Period{
	"1m":    {Name: "1m", Category: types.KLINE_TYPE_1MIN, IndexCategory: types.KLINE_TYPE_1MIN, Times: 1, Kind: periodMinute, Duration: time.Minute},
	"3m":    {Name: "3m", Category: types.KLINE_TYPE_1MIN, IndexCategory: types.KLINE_TYPE_1MIN, Times: 3, Kind: periodMinute, Duration: 3 * time.Minute},
	"5m":    {Name: "5m", Category: types.KLINE_TYPE_5MIN, IndexCategory: types.KLINE_TYPE_5MIN, Times: 1, Kind: periodMinute, Duration: 5 * time.Minute},
	"15m":   {Name: "15m", Category: types.KLINE_TYPE_15MIN, IndexCategory: types.KLINE_TYPE_15MIN, Times: 1, Kind: periodMinute, Duration: 15 * time.Minute},
	"30m":   {Name: "30m", Category: types.KLINE_TYPE_30MIN, IndexCategory: types.KLINE_TYPE_30MIN, Times: 1, Kind: periodMinute, Duration: 30 * time.Minute},
	"60m":   {Name: "60m", Category: types.KLINE_TYPE_1HOUR, IndexCategory: types.KLINE_TYPE_1HOUR, Times: 1, Kind: periodMinute, Duration: time.Hour},
	"day":   {Name: "day", Category: types.KLINE_TYPE_DAILY, IndexCategory: types.KLINE_TYPE_RI_K, Times: 1, Kind: periodDay},
	"week":  {Name: "week", Category: types.KLINE_TYPE_WEEKLY, IndexCategory: types.KLINE_TYPE_WEEKLY, Times: 1, Kind: periodWeek},
	"month": {Name: "month", Category: types.KLINE_TYPE_MONTHLY, IndexCategory: types.KLINE_TYPE_MONTHLY, Times: 1, Kind: periodMonth},
	"year":  {Name: "year", Category: types.KLINE_TYPE_YEARLY, IndexCategory: types.KLINE_TYPE_YEARLY, Times: 1, Kind: periodYear},
}

// Query 是完成一次单股票时间区间 K 线查询所需的规范化参数。
type Query struct {
	Type       assetType
	Code       string
	Market     uint8
	Period     Period
	Start      time.Time
	End        time.Time
	Adjust     uint16
	AdjustName string
}

// Bar 是 HTTP 接口输出的稳定 K 线结构。
type Bar struct {
	Time      string   `json:"time"`
	Open      float64  `json:"open"`
	High      float64  `json:"high"`
	Low       float64  `json:"low"`
	Close     float64  `json:"close"`
	Volume    float64  `json:"volume"`
	Amount    float64  `json:"amount"`
	Turnover  *float64 `json:"turnover"`
	Previous  float64  `json:"prev_close"`
	Change    float64  `json:"change"`
	ChangePct float64  `json:"change_pct"`
}

// parseAssetType 解析证券类型，空值默认按股票处理。
func parseAssetType(value string) (assetType, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", string(assetStock):
		return assetStock, nil
	case string(assetIndex):
		return assetIndex, nil
	default:
		return "", fmt.Errorf("type 仅支持 stock、index")
	}
}

// parsePeriod 把外部周期名称转换为 TDX 的周期和倍数参数。
func parsePeriod(value string) (Period, error) {
	period, ok := supportedPeriods[strings.ToLower(strings.TrimSpace(value))]
	if !ok {
		return Period{}, fmt.Errorf("period 仅支持 1m、3m、5m、15m、30m、60m、day、week、month、year")
	}
	return period, nil
}

// parseAdjust 解析复权名称，空值默认使用前复权。
func parseAdjust(value string) (string, uint16, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "qfq":
		return "qfq", types.AdjustQFQ, nil
	case "none":
		return "none", types.AdjustNone, nil
	case "hfq":
		return "hfq", types.AdjustHFQ, nil
	default:
		return "", 0, fmt.Errorf("adjust 仅支持 none、qfq、hfq")
	}
}

// marketForCode 校验沪深北 A 股六位代码并推断 TDX 市场号。
func marketForCode(code string) (uint8, error) {
	if len(code) != 6 || !isASCIIDigits(code) {
		return 0, fmt.Errorf("code 必须是沪深北 A 股六位代码")
	}
	switch {
	case strings.HasPrefix(code, "60"), strings.HasPrefix(code, "68"):
		return 1, nil
	case strings.HasPrefix(code, "00"), strings.HasPrefix(code, "30"):
		return 0, nil
	case strings.HasPrefix(code, "4"), strings.HasPrefix(code, "8"), strings.HasPrefix(code, "92"):
		return 2, nil
	default:
		return 0, fmt.Errorf("code 必须是沪深北 A 股六位代码")
	}
}

// parseIndexMarket 把指数请求中的市场名称转换为 TDX 市场号。
func parseIndexMarket(value string) (uint8, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "sh":
		return 1, nil
	case "sz":
		return 0, nil
	case "bj":
		return 2, nil
	default:
		return 0, fmt.Errorf("type=index 时 market 必填且仅支持 sh、sz、bj")
	}
}

// validateSixDigitCode 校验股票或指数代码是否为六位数字。
func validateSixDigitCode(code string) error {
	if len(code) != 6 || !isASCIIDigits(code) {
		return fmt.Errorf("code 必须是六位数字")
	}
	return nil
}

// parseRangeTime 使用北京时间解析日期或日期时间，日期型 end 覆盖到当天结束。
func parseRangeTime(value string, endOfDay bool) (time.Time, error) {
	text := strings.TrimSpace(value)
	if text == "" {
		return time.Time{}, fmt.Errorf("时间不能为空")
	}
	if parsed, err := time.ParseInLocation(dateTimeLayout, text, shanghaiLocation); err == nil {
		return parsed, nil
	}
	parsed, err := time.ParseInLocation("2006-01-02", text, shanghaiLocation)
	if err != nil {
		return time.Time{}, fmt.Errorf("时间格式必须是 YYYY-MM-DD 或 YYYY-MM-DD HH:mm:ss")
	}
	if endOfDay {
		return parsed.Add(24*time.Hour - time.Nanosecond), nil
	}
	return parsed, nil
}

// wallTimeInShanghai 保留协议时间的年月日时分字段并固定解释为北京时间。
func wallTimeInShanghai(value time.Time) time.Time {
	return time.Date(value.Year(), value.Month(), value.Day(), value.Hour(), value.Minute(), value.Second(), 0, shanghaiLocation)
}

// sameDate 判断两个时间是否属于同一自然日。
func sameDate(left time.Time, right time.Time) bool {
	return left.Year() == right.Year() && left.YearDay() == right.YearDay()
}

// isASCIIDigits 判断文本是否完全由 ASCII 数字组成。
func isASCIIDigits(value string) bool {
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return value != ""
}
