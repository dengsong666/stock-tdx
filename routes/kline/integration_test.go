package kline

import (
	"os"
	"testing"
	"time"

	"github.com/bensema/gotdx/types"
)

func TestKLineServiceIntegration(t *testing.T) {
	if os.Getenv("GOTDX_INTEGRATION") != "1" {
		t.Skip("set GOTDX_INTEGRATION=1 to run live TDX K-line integration test")
	}
	now := time.Now().In(shanghaiLocation)
	query := Query{
		Code:       "600519",
		Market:     1,
		Period:     supportedPeriods["3m"],
		Start:      now.AddDate(0, 0, -14),
		End:        now,
		Adjust:     types.AdjustQFQ,
		AdjustName: "qfq",
	}
	bars, err := NewService().Fetch(query)
	if err != nil {
		t.Fatalf("live 3m query failed: %v", err)
	}
	if len(bars) == 0 {
		t.Fatal("live 3m query returned no bars")
	}
	for _, bar := range bars {
		parsed, err := time.ParseInLocation(dateTimeLayout, bar.Time, shanghaiLocation)
		if err != nil || parsed.Before(query.Start) || parsed.After(query.End) || bar.Open <= 0 || bar.High <= 0 || bar.Low <= 0 || bar.Close <= 0 {
			t.Fatalf("invalid live bar: %#v err=%v", bar, err)
		}
	}
	if !looksLikeThreeMinuteBars(bars) {
		t.Fatalf("TDX host did not honor 1m x 3 period, sample=%#v", bars[:min(len(bars), 5)])
	}
}

// looksLikeThreeMinuteBars 确认同一交易时段的相邻数据没有退化成 1 分钟线。
func looksLikeThreeMinuteBars(bars []Bar) bool {
	for index := 1; index < len(bars); index++ {
		previous, previousErr := time.ParseInLocation(dateTimeLayout, bars[index-1].Time, shanghaiLocation)
		current, currentErr := time.ParseInLocation(dateTimeLayout, bars[index].Time, shanghaiLocation)
		if previousErr != nil || currentErr != nil || !sameDate(previous, current) {
			continue
		}
		delta := current.Sub(previous)
		if delta == time.Minute || delta == 2*time.Minute {
			return false
		}
	}
	return true
}
