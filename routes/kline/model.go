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

const (
	periodMinute periodKind = iota
	periodDay
	periodWeek
	periodMonth
	periodYear
)

// Period 描述外部周期名称及其 TDX 协议参数。
type Period struct {
	Name     string
	Category uint16
	Times    uint16
	Kind     periodKind
	Duration time.Duration
}

var supportedPeriods = map[string]Period{
	"1m":    {Name: "1m", Category: types.KLINE_TYPE_1MIN, Times: 1, Kind: periodMinute, Duration: time.Minute},
	"3m":    {Name: "3m", Category: types.KLINE_TYPE_1MIN, Times: 3, Kind: periodMinute, Duration: 3 * time.Minute},
	"5m":    {Name: "5m", Category: types.KLINE_TYPE_5MIN, Times: 1, Kind: periodMinute, Duration: 5 * time.Minute},
	"15m":   {Name: "15m", Category: types.KLINE_TYPE_15MIN, Times: 1, Kind: periodMinute, Duration: 15 * time.Minute},
	"30m":   {Name: "30m", Category: types.KLINE_TYPE_30MIN, Times: 1, Kind: periodMinute, Duration: 30 * time.Minute},
	"60m":   {Name: "60m", Category: types.KLINE_TYPE_1HOUR, Times: 1, Kind: periodMinute, Duration: time.Hour},
	"day":   {Name: "day", Category: types.KLINE_TYPE_DAILY, Times: 1, Kind: periodDay},
	"week":  {Name: "week", Category: types.KLINE_TYPE_WEEKLY, Times: 1, Kind: periodWeek},
	"month": {Name: "month", Category: types.KLINE_TYPE_MONTHLY, Times: 1, Kind: periodMonth},
	"year":  {Name: "year", Category: types.KLINE_TYPE_YEARLY, Times: 1, Kind: periodYear},
}

// Query 是完成一次单股票时间区间 K 线查询所需的规范化参数。
type Query struct {
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
	Time       string  `json:"time"`
	Open       float64 `json:"open"`
	High       float64 `json:"high"`
	Low        float64 `json:"low"`
	Close      float64 `json:"close"`
	Volume     float64 `json:"volume"`
	Amount     float64 `json:"amount"`
	Turnover   float64 `json:"turnover"`
	Previous   float64 `json:"prev_close"`
	Change     float64 `json:"change"`
	ChangePct  float64 `json:"change_pct"`
	IsComplete bool    `json:"is_complete"`
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

// isBarComplete 判断当前周期是否已经结束。
func isBarComplete(period Period, barTime time.Time, now time.Time) bool {
	barTime = wallTimeInShanghai(barTime)
	now = now.In(shanghaiLocation)
	switch period.Kind {
	case periodMinute:
		return !now.Before(barTime)
	case periodDay:
		if beforeDate(barTime, now) {
			return true
		}
		return sameDate(barTime, now) && !now.Before(marketClose(now))
	case periodWeek:
		barYear, barWeek := barTime.ISOWeek()
		nowYear, nowWeek := now.ISOWeek()
		if barYear != nowYear || barWeek != nowWeek {
			return barTime.Before(now)
		}
		weekdayFromMonday := (int(now.Weekday()) + 6) % 7
		fridayClose := marketClose(now.AddDate(0, 0, 4-weekdayFromMonday))
		return !now.Before(fridayClose)
	case periodMonth:
		if barTime.Year() != now.Year() || barTime.Month() != now.Month() {
			return barTime.Before(now)
		}
		lastDay := time.Date(now.Year(), now.Month()+1, 0, 15, 0, 0, 0, shanghaiLocation)
		return !now.Before(lastDay)
	case periodYear:
		if barTime.Year() != now.Year() {
			return barTime.Before(now)
		}
		lastDay := time.Date(now.Year(), time.December, 31, 15, 0, 0, 0, shanghaiLocation)
		return !now.Before(lastDay)
	default:
		return false
	}
}

// marketClose 返回指定日期的北京时间 15:00。
func marketClose(value time.Time) time.Time {
	return time.Date(value.Year(), value.Month(), value.Day(), 15, 0, 0, 0, shanghaiLocation)
}

// beforeDate 判断 left 的自然日是否早于 right。
func beforeDate(left time.Time, right time.Time) bool {
	leftDate := time.Date(left.Year(), left.Month(), left.Day(), 0, 0, 0, 0, shanghaiLocation)
	rightDate := time.Date(right.Year(), right.Month(), right.Day(), 0, 0, 0, 0, shanghaiLocation)
	return leftDate.Before(rightDate)
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
