package kline

import (
	"errors"
	"testing"
	"time"

	"github.com/bensema/gotdx/proto"
	"github.com/bensema/gotdx/types"
)

type barsCall struct {
	market uint8
	code   string
	period uint16
	times  uint16
	start  uint32
	count  uint32
	adjust uint16
}

type fakeBarsClient struct {
	items        []proto.MACSymbolBar
	err          error
	calls        []barsCall
	disconnected bool
}

// MACSymbolBars 按“最新到最旧”的偏移模型返回离线 K 线。
func (client *fakeBarsClient) MACSymbolBars(market uint8, code string, period uint16, times uint16, start uint32, count uint32, adjust uint16) ([]proto.MACSymbolBar, error) {
	client.calls = append(client.calls, barsCall{market: market, code: code, period: period, times: times, start: start, count: count, adjust: adjust})
	if client.err != nil {
		return nil, client.err
	}
	if start >= uint32(len(client.items)) {
		return nil, nil
	}
	end := start + count
	if end > uint32(len(client.items)) {
		end = uint32(len(client.items))
	}
	return append([]proto.MACSymbolBar(nil), client.items[start:end]...), nil
}

// Disconnect 记录服务是否正确关闭短连接。
func (client *fakeBarsClient) Disconnect() error {
	client.disconnected = true
	return nil
}

func TestServiceFetchRangeAndThreeMinuteMapping(t *testing.T) {
	day := time.Date(2026, 7, 24, 0, 0, 0, 0, shanghaiLocation)
	client := &fakeBarsClient{items: []proto.MACSymbolBar{
		barAtTime(day.Add(10*time.Hour), 10.0),
		barAtTime(day.Add(9*time.Hour+59*time.Minute), 9.9),
		barAtTime(day.Add(9*time.Hour+58*time.Minute), 9.8),
		barAtTime(day.Add(9*time.Hour+57*time.Minute), 9.7),
		barAtTime(day.Add(9*time.Hour+56*time.Minute), 9.6),
	}}
	service := newService([]string{"host-a"}, 3, func(string, int) macBarsClient { return client }, func() time.Time {
		return day.Add(10*time.Hour + time.Minute)
	})
	query := Query{
		Code:       "600519",
		Market:     1,
		Period:     supportedPeriods["3m"],
		Start:      day.Add(9*time.Hour + 57*time.Minute),
		End:        day.Add(9*time.Hour + 59*time.Minute),
		Adjust:     types.AdjustQFQ,
		AdjustName: "qfq",
	}

	bars, err := service.Fetch(query)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(bars) != 3 || bars[0].Time != "2026-07-24 09:57:00" || bars[2].Time != "2026-07-24 09:59:00" {
		t.Fatalf("unexpected range: %#v", bars)
	}
	if !bars[2].IsComplete {
		t.Fatalf("historical minute bar must be complete: %#v", bars[2])
	}
	if !client.disconnected {
		t.Fatal("short connection was not disconnected")
	}
	for _, call := range client.calls {
		if call.market != 1 || call.code != "600519" || call.period != types.KLINE_TYPE_1MIN || call.times != 3 || call.adjust != types.AdjustQFQ {
			t.Fatalf("unexpected protocol mapping: %#v", call)
		}
	}
}

func TestServiceHostFailover(t *testing.T) {
	failed := &fakeBarsClient{err: errors.New("dial failed")}
	succeeded := &fakeBarsClient{items: []proto.MACSymbolBar{barAtTime(time.Date(2026, 7, 24, 15, 0, 0, 0, shanghaiLocation), 10)}}
	clients := []macBarsClient{failed, succeeded}
	index := 0
	service := newService([]string{"host-a", "host-b"}, 3, func(string, int) macBarsClient {
		client := clients[index]
		index++
		return client
	}, time.Now)
	query := Query{
		Code:   "300750",
		Market: 0,
		Period: supportedPeriods["day"],
		Start:  time.Date(2026, 7, 1, 0, 0, 0, 0, shanghaiLocation),
		End:    time.Date(2026, 7, 26, 0, 0, 0, 0, shanghaiLocation),
		Adjust: types.AdjustQFQ,
	}
	bars, err := service.Fetch(query)
	if err != nil || len(bars) != 1 {
		t.Fatalf("failover result bars=%#v err=%v", bars, err)
	}
	if !failed.disconnected || !succeeded.disconnected {
		t.Fatal("all attempted short connections must be disconnected")
	}
}

func TestServiceRejectsMoreThanTwentyThousandBars(t *testing.T) {
	latest := time.Date(2026, 7, 24, 15, 0, 0, 0, shanghaiLocation)
	items := make([]proto.MACSymbolBar, maxBarsPerRequest+1)
	for index := range items {
		items[index] = barAtTime(latest.Add(-time.Duration(index)*time.Minute), float64(index+1))
	}
	client := &fakeBarsClient{items: items}
	service := newService([]string{"host"}, 3, func(string, int) macBarsClient { return client }, time.Now)
	query := Query{
		Code:   "000001",
		Market: 0,
		Period: supportedPeriods["1m"],
		Start:  latest.Add(-time.Duration(len(items)) * time.Minute),
		End:    latest,
		Adjust: types.AdjustQFQ,
	}
	if _, err := service.Fetch(query); !errors.Is(err, ErrTooManyBars) {
		t.Fatalf("expected ErrTooManyBars, got %v", err)
	}
}

func TestMarketForCodeOnlyAcceptsAShares(t *testing.T) {
	for code, want := range map[string]uint8{
		"600519": 1,
		"688981": 1,
		"000001": 0,
		"300750": 0,
		"920001": 2,
		"830799": 2,
	} {
		got, err := marketForCode(code)
		if err != nil || got != want {
			t.Fatalf("code=%s got=%d want=%d err=%v", code, got, want, err)
		}
	}
	for _, code := range []string{"510300", "110001", "200001", "ABCDEF", "60051"} {
		if _, err := marketForCode(code); err == nil {
			t.Fatalf("expected %s to be rejected", code)
		}
	}
}

func TestIsBarComplete(t *testing.T) {
	now := time.Date(2026, 7, 24, 14, 30, 0, 0, shanghaiLocation)
	if !isBarComplete(supportedPeriods["1m"], now.Add(-time.Minute), now) {
		t.Fatal("past minute must be complete")
	}
	if isBarComplete(supportedPeriods["1m"], now.Add(time.Minute), now) {
		t.Fatal("future minute must be incomplete")
	}
	if isBarComplete(supportedPeriods["day"], now, now) {
		t.Fatal("current day before close must be incomplete")
	}
	if !isBarComplete(supportedPeriods["day"], now.AddDate(0, 0, -1), now) {
		t.Fatal("previous day must be complete")
	}
	sunday := time.Date(2026, 7, 26, 10, 0, 0, 0, shanghaiLocation)
	if !isBarComplete(supportedPeriods["week"], now, sunday) {
		t.Fatal("current ISO week must be complete after Friday close")
	}
}

// barAtTime 创建服务测试使用的最小 K 线。
func barAtTime(dateTime time.Time, closePrice float64) proto.MACSymbolBar {
	return proto.MACSymbolBar{
		DateTime: dateTime,
		Open:     closePrice - 0.1,
		High:     closePrice + 0.2,
		Low:      closePrice - 0.2,
		Close:    closePrice,
		Vol:      100,
		Amount:   1000,
		Turnover: 0.1,
		PreClose: closePrice - 0.05,
	}
}
