package unusual

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bensema/gotdx/proto"
	"github.com/bensema/gotdx/types"
)

type monitorResult struct {
	items []proto.MACMarketMonitorItem
	err   error
}

type monitorCall struct {
	market uint8
	start  uint32
	count  uint32
}

// sequenceMonitorClient 按市场依次返回预设结果，用于驱动采集循环。
type sequenceMonitorClient struct {
	mu          sync.Mutex
	sequences   map[uint8][]monitorResult
	calls       map[uint8]int
	callHistory []monitorCall
	disconnects int
}

func (client *sequenceMonitorClient) MACMarketMonitor(market uint8, start uint32, count uint32) ([]proto.MACMarketMonitorItem, error) {
	if count != stockUnusualMonitorCount {
		return nil, fmt.Errorf("unexpected monitor count: %d", count)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.calls == nil {
		client.calls = make(map[uint8]int)
	}
	index := client.calls[market]
	client.calls[market]++
	client.callHistory = append(client.callHistory, monitorCall{market: market, start: start, count: count})
	sequence := client.sequences[market]
	if index >= len(sequence) {
		return nil, nil
	}
	result := sequence[index]
	return append([]proto.MACMarketMonitorItem(nil), result.items...), result.err
}

func (client *sequenceMonitorClient) Disconnect() error {
	client.mu.Lock()
	client.disconnects++
	client.mu.Unlock()
	return nil
}

func (client *sequenceMonitorClient) callCount(market uint8) int {
	client.mu.Lock()
	defer client.mu.Unlock()
	return client.calls[market]
}

func (client *sequenceMonitorClient) history() ([]monitorCall, int) {
	client.mu.Lock()
	defer client.mu.Unlock()
	return append([]monitorCall(nil), client.callHistory...), client.disconnects
}

// pagedMonitorClient 返回 total 条递增序号的异动，用于验证分页与二分定位。
type pagedMonitorClient struct {
	mu          sync.Mutex
	total       uint32
	calls       []monitorCall
	disconnects int
}

func (client *pagedMonitorClient) MACMarketMonitor(market uint8, start uint32, count uint32) ([]proto.MACMarketMonitorItem, error) {
	client.mu.Lock()
	client.calls = append(client.calls, monitorCall{market: market, start: start, count: count})
	client.mu.Unlock()
	if start >= client.total {
		return nil, nil
	}
	length := count
	if remaining := client.total - start; remaining < length {
		length = remaining
	}
	items := make([]proto.MACMarketMonitorItem, length)
	for index := range items {
		items[index].Index = uint16(start + uint32(index))
		items[index].Code = "000001"
	}
	return items, nil
}

func (client *pagedMonitorClient) Disconnect() error {
	client.mu.Lock()
	client.disconnects++
	client.mu.Unlock()
	return nil
}

func (client *pagedMonitorClient) stats() ([]monitorCall, int) {
	client.mu.Lock()
	defer client.mu.Unlock()
	return append([]monitorCall(nil), client.calls...), client.disconnects
}

func TestParseStockTargets(t *testing.T) {
	targets, err := parseStockTargets(" 000001,600000,920001,000001 ")
	if err != nil {
		t.Fatalf("parse targets failed: %v", err)
	}
	if len(targets) != 3 {
		t.Fatalf("unexpected target count: %d", len(targets))
	}
	for _, target := range []stockTarget{
		{market: types.MarketSZ.Uint8(), code: "000001"},
		{market: types.MarketSH.Uint8(), code: "600000"},
		{market: types.MarketBJ.Uint8(), code: "920001"},
	} {
		if _, ok := targets[target]; !ok {
			t.Fatalf("missing target: %+v", target)
		}
	}

	invalid := []string{
		"",
		"000001,,600000",
		"000001,ABCDEF",
		"12345",
		"123456",
		"510300",
	}
	for _, value := range invalid {
		t.Run(value, func(t *testing.T) {
			if _, err := parseStockTargets(value); err == nil {
				t.Fatalf("expected invalid stocks error for %q", value)
			}
		})
	}

	many := make([]string, 0, stockUnusualMaxStocks+1)
	for i := 0; i <= stockUnusualMaxStocks; i++ {
		many = append(many, fmt.Sprintf("%06d", i))
	}
	if _, err := parseStockTargets(strings.Join(many, ",")); err == nil {
		t.Fatal("expected too many stocks error")
	}
}

func TestParseUnusualSince(t *testing.T) {
	if _, has, err := parseUnusualSince("  "); err != nil || has {
		t.Fatalf("empty since should be treated as absent: has=%v err=%v", has, err)
	}
	if seq, has, err := parseUnusualSince("42"); err != nil || !has || seq != 42 {
		t.Fatalf("unexpected since parse: seq=%d has=%v err=%v", seq, has, err)
	}
	for _, value := range []string{"-1", "abc", "1.5"} {
		if _, _, err := parseUnusualSince(value); err == nil {
			t.Fatalf("expected invalid since error for %q", value)
		}
	}
}

func TestStockUnusualSSEOptions(t *testing.T) {
	config := stockUnusualSSEConfig{
		pollInterval:      defaultStockUnusualPollInterval,
		heartbeatInterval: defaultStockUnusualHeartbeatInterval,
		autoStart:         true,
	}
	WithPollInterval(3 * time.Second)(&config)
	WithHeartbeatInterval(2 * time.Minute)(&config)
	WithCollectorAutoStart(false)(&config)
	if config.pollInterval != 3*time.Second || config.heartbeatInterval != 2*time.Minute || config.autoStart {
		t.Fatalf("unexpected options: %+v", config)
	}
	WithPollInterval(0)(&config)
	WithHeartbeatInterval(-time.Second)(&config)
	if config.pollInterval != 3*time.Second || config.heartbeatInterval != 2*time.Minute {
		t.Fatalf("invalid options should be ignored: %+v", config)
	}
}

func TestInStockUnusualTradingWindow(t *testing.T) {
	cases := []struct {
		name string
		when time.Time
		want bool
	}{
		{"周一开盘前", time.Date(2026, 11, 23, 9, 14, 0, 0, shanghaiLocation), false},
		{"周一竞价开始", time.Date(2026, 11, 23, 9, 15, 0, 0, shanghaiLocation), true},
		{"周一午休", time.Date(2026, 11, 23, 12, 0, 0, 0, shanghaiLocation), true},
		{"周一收盘边界", time.Date(2026, 11, 23, 15, 5, 0, 0, shanghaiLocation), true},
		{"周一收盘后", time.Date(2026, 11, 23, 15, 6, 0, 0, shanghaiLocation), false},
		{"周六", time.Date(2026, 11, 28, 10, 0, 0, 0, shanghaiLocation), false},
		{"周日", time.Date(2026, 11, 29, 10, 0, 0, 0, shanghaiLocation), false},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			if got := inStockUnusualTradingWindow(item.when); got != item.want {
				t.Fatalf("window=%v want=%v for %s", got, item.want, item.when)
			}
		})
	}
}

func TestQueryMarketMonitorRetriesTransientErrors(t *testing.T) {
	market := types.MarketSZ.Uint8()
	want := testUnusualItem(42, "10:30:00", "加速拉升")
	client := &sequenceMonitorClient{sequences: map[uint8][]monitorResult{
		market: {
			{err: errors.New("第一次超时")},
			{err: errors.New("第二次超时")},
			{items: []proto.MACMarketMonitorItem{want}},
		},
	}}
	handler := newStockUnusualSSEHandler(client, testSSEConfig())

	items, err := handler.queryMarketMonitor(market, 600, stockUnusualMonitorCount)
	if err != nil {
		t.Fatalf("transient failures should recover: %v", err)
	}
	if len(items) != 1 || items[0].Index != want.Index {
		t.Fatalf("unexpected recovered items: %+v", items)
	}
	history, _ := client.history()
	if len(history) != stockUnusualPollAttempts {
		t.Fatalf("unexpected retry stats: calls=%d", len(history))
	}
}

func TestQueryMarketMonitorReturnsErrorAfterRetries(t *testing.T) {
	market := types.MarketSH.Uint8()
	client := &sequenceMonitorClient{sequences: map[uint8][]monitorResult{
		market: {
			{err: errors.New("持续超时")},
			{err: errors.New("持续超时")},
			{err: errors.New("持续超时")},
		},
	}}
	handler := newStockUnusualSSEHandler(client, testSSEConfig())

	_, err := handler.queryMarketMonitor(market, 1200, stockUnusualMonitorCount)
	if err == nil || !strings.Contains(err.Error(), "failed after 3 attempts") {
		t.Fatalf("expected exhausted retry error, got: %v", err)
	}
	history, _ := client.history()
	if len(history) != stockUnusualPollAttempts {
		t.Fatalf("unexpected retry stats: calls=%d", len(history))
	}
}

func TestPollMarketBackfillsOnFirstRun(t *testing.T) {
	client := &pagedMonitorClient{total: 100}
	handler := newStockUnusualSSEHandler(client, testSSEConfig())
	market := types.MarketSZ.Uint8()
	subscriber := attachTestSubscriber(handler, map[stockTarget]struct{}{
		{market: market, code: "000001"}: {},
	})
	defer handler.unsubscribe(subscriber)

	handler.pollMarket(market, tradingNow())

	handler.mu.Lock()
	buffered := len(handler.events)
	cursor := handler.cursors[market]
	initialized := handler.initialized[market]
	handler.mu.Unlock()

	if buffered != 100 || cursor != 100 || !initialized {
		t.Fatalf("unexpected backfill result: buffered=%d cursor=%d initialized=%v", buffered, cursor, initialized)
	}
	if got := handler.CurrentSeq() - handler.BaseSeq(); got != 99 {
		t.Fatalf("seq should advance by backfilled events, got delta=%d", got)
	}
	// 回填期间已经连上的客户端必须收到这些事件：服务重启后 app 正好在连，
	// 若只填缓冲不广播，它会缺掉「订阅之后才回填出来的部分」。
	if len(subscriber.messages) != 100 {
		t.Fatalf("backfill must broadcast to live subscribers, got %d", len(subscriber.messages))
	}
}

func TestPollMarketDrainsFullPages(t *testing.T) {
	firstPage := make([]proto.MACMarketMonitorItem, stockUnusualMonitorCount)
	for index := range firstPage {
		firstPage[index] = testUnusualItem(uint16(1000+index), "14:30:00", fmt.Sprintf("异动%d", index))
	}
	lastPage := []proto.MACMarketMonitorItem{
		testUnusualItem(1600, "14:30:01", "新增1"),
		testUnusualItem(1601, "14:30:02", "新增2"),
	}
	market := types.MarketSZ.Uint8()
	client := &sequenceMonitorClient{sequences: map[uint8][]monitorResult{
		market: {
			{items: firstPage},
			{items: lastPage},
		},
	}}
	handler := newStockUnusualSSEHandler(client, testSSEConfig())
	today := handler.now().In(shanghaiLocation).Format(time.DateOnly)
	handler.mu.Lock()
	handler.date = today
	handler.initialized[market] = true
	handler.cursors[market] = 1000
	handler.mu.Unlock()

	handler.pollMarket(market, handler.now())

	handler.mu.Lock()
	cursor := handler.cursors[market]
	buffered := len(handler.events)
	handler.mu.Unlock()
	if cursor != 1602 {
		t.Fatalf("unexpected cursor after draining pages: %d", cursor)
	}
	if buffered != int(stockUnusualMonitorCount)+2 {
		t.Fatalf("unexpected buffered events: %d", buffered)
	}
	history, _ := client.history()
	if len(history) != 2 || history[0].start != 1000 || history[1].start != 1600 {
		t.Fatalf("unexpected drain history: %+v", history)
	}
}

func TestStockUnusualSSEHTTPValidation(t *testing.T) {
	mux := http.NewServeMux()
	RegisterStockUnusualSSE(mux, &sequenceMonitorClient{}, WithCollectorAutoStart(false))

	request := httptest.NewRequest(http.MethodPost, StockUnusualSSEPath+"?stocks=000001", nil)
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusMethodNotAllowed || recorder.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("unexpected method response: code=%d headers=%v", recorder.Code, recorder.Header())
	}

	request = httptest.NewRequest(http.MethodGet, StockUnusualSSEPath+"?stocks=bad", nil)
	recorder = httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "6 位数字") {
		t.Fatalf("unexpected validation response: code=%d body=%s", recorder.Code, recorder.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, StockUnusualSSEPath+"?stocks=000001&since=abc", nil)
	recorder = httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "since") {
		t.Fatalf("unexpected since validation response: code=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestStockUnusualSSEStreamsNewItemsAfterReady(t *testing.T) {
	market := types.MarketSZ.Uint8()
	newItem1 := testUnusualItem(1, "09:31:00", "大单托盘")
	newItem2 := testUnusualItem(2, "09:32:00", "主力买入")
	client := &sequenceMonitorClient{
		sequences: map[uint8][]monitorResult{
			market: {
				{items: nil},
				{items: []proto.MACMarketMonitorItem{newItem1, newItem2}},
			},
		},
	}
	handler := newStockUnusualSSEHandler(client, stockUnusualSSEConfig{
		pollInterval:      time.Hour,
		heartbeatInterval: time.Hour,
	})
	handler.now = tradingNow
	mux := http.NewServeMux()
	mux.Handle(StockUnusualSSEPath, handler)
	server := httptest.NewServer(mux)
	defer server.Close()
	defer handler.Close()

	response, cancel := openSSE(t, server.URL+StockUnusualSSEPath+"?stocks=000001")
	defer cancel()
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status: %d", response.StatusCode)
	}
	if got := response.Header.Get("Content-Type"); got != "text/event-stream; charset=utf-8" {
		t.Fatalf("unexpected content type: %s", got)
	}
	if got := response.Header.Get("X-Accel-Buffering"); got != "no" {
		t.Fatalf("unexpected buffering header: %s", got)
	}

	reader := bufio.NewReader(response.Body)
	ready := readSSEEvent(t, reader, time.Second)
	if ready.event != "ready" {
		t.Fatalf("first event must be ready, got %+v", ready)
	}
	var readyPayload struct {
		Seq uint64 `json:"seq"`
	}
	if err := json.Unmarshal(ready.data, &readyPayload); err != nil {
		t.Fatalf("decode ready failed: %v", err)
	}

	eventually(t, time.Second, func() bool {
		handler.mu.Lock()
		defer handler.mu.Unlock()
		return handler.initialized[market]
	})
	handler.pollMarket(market, tradingNow())

	first := readSSEEvent(t, reader, 2*time.Second)
	second := readSSEEvent(t, reader, 2*time.Second)
	if first.event != "unusual" || second.event != "unusual" {
		t.Fatalf("unexpected events: %+v %+v", first, second)
	}

	var firstPayload map[string]any
	if err := json.Unmarshal(first.data, &firstPayload); err != nil {
		t.Fatalf("decode first event failed: %v", err)
	}
	if firstPayload["code"] != "000001" || firstPayload["desc"] != newItem1.Desc || firstPayload["type"] != float64(newItem1.UnusualType) {
		t.Fatalf("unexpected first payload: %+v", firstPayload)
	}
	if firstPayload["seq"] != float64(readyPayload.Seq+1) {
		t.Fatalf("first event seq must follow ready seq: ready=%d payload=%+v", readyPayload.Seq, firstPayload)
	}
	if _, exists := firstPayload["market"]; exists {
		t.Fatalf("market must not be exposed: %+v", firstPayload)
	}
	if _, exists := firstPayload["index"]; exists {
		t.Fatalf("index must not be exposed: %+v", firstPayload)
	}
	if _, exists := firstPayload["raw"]; !exists {
		t.Fatalf("raw values must be exposed: %+v", firstPayload)
	}

	var secondPayload unusualEventPayload
	if err := json.Unmarshal(second.data, &secondPayload); err != nil {
		t.Fatalf("decode second event failed: %v", err)
	}
	if secondPayload.Desc != newItem2.Desc {
		t.Fatalf("response order changed: %+v", secondPayload)
	}
}

func TestStockUnusualSSEReplaysSinceCursor(t *testing.T) {
	handler := newStockUnusualSSEHandler(&sequenceMonitorClient{}, testSSEConfig())
	handler.now = tradingNow
	market := types.MarketSZ.Uint8()
	handler.mu.Lock()
	handler.date = handler.now().In(shanghaiLocation).Format(time.DateOnly)
	handler.initialized[market] = true
	handler.cursors[market] = 0
	handler.mu.Unlock()

	// 游标起点是启动时刻的时间戳，测试一律用相对值，不能假设从 1 开始。
	startSeq := handler.CurrentSeq()
	items := []proto.MACMarketMonitorItem{
		testUnusualItem(1, "09:30:00", "第一"),
		testUnusualItem(2, "09:30:02", "第二"),
		testUnusualItem(3, "09:30:04", "第三"),
	}
	handler.ingest(market, 0, items, true)

	subscriber, replay, resetSeq, readySeq := handler.subscribe(map[stockTarget]struct{}{
		{market: market, code: "000001"}: {},
	}, startSeq+1, true)
	defer handler.unsubscribe(subscriber)

	if resetSeq != 0 {
		t.Fatalf("since inside buffer must not reset: %d", resetSeq)
	}
	if readySeq != startSeq+3 || len(replay) != 2 {
		t.Fatalf("expected two replayed events, got %d (ready=%d)", len(replay), readySeq)
	}
	var payload unusualEventPayload
	if err := json.Unmarshal(replay[0].data, &payload); err != nil {
		t.Fatalf("decode replay failed: %v", err)
	}
	if payload.Seq != startSeq+2 || payload.Desc != "第二" {
		t.Fatalf("unexpected first replay: %+v", payload)
	}
}

func TestSubscribeWithoutSinceReplaysWholeBuffer(t *testing.T) {
	handler := newStockUnusualSSEHandler(&sequenceMonitorClient{}, testSSEConfig())
	market := types.MarketSZ.Uint8()
	handler.mu.Lock()
	handler.date = handler.now().In(shanghaiLocation).Format(time.DateOnly)
	handler.initialized[market] = true
	handler.mu.Unlock()

	startSeq := handler.CurrentSeq()
	handler.ingest(market, 0, []proto.MACMarketMonitorItem{
		testUnusualItem(1, "09:30:00", "第一"),
		testUnusualItem(2, "09:31:00", "第二"),
		testUnusualItem(3, "09:32:00", "第三"),
	}, true)

	// app 可能是下午才第一次打开：首连必须能拿到当天已经发生的全部异动。
	subscriber, replay, resetSeq, readySeq := handler.subscribe(map[stockTarget]struct{}{
		{market: market, code: "000001"}: {},
	}, 0, false)
	defer handler.unsubscribe(subscriber)

	if resetSeq != 0 {
		t.Fatalf("首连不应触发 reset，got %d", resetSeq)
	}
	if len(replay) != 3 {
		t.Fatalf("首连应补发当天全量，got %d", len(replay))
	}
	if readySeq != startSeq+3 {
		t.Fatalf("ready 游标应为当前尾部，got %d want %d", readySeq, startSeq+3)
	}
	var first unusualEventPayload
	if err := json.Unmarshal(replay[0].data, &first); err != nil {
		t.Fatalf("decode replay failed: %v", err)
	}
	if first.Seq != startSeq+1 || first.Desc != "第一" {
		t.Fatalf("补发顺序应为入库顺序: %+v", first)
	}
}

func TestSubscribeWithoutSinceSkipsOtherStocks(t *testing.T) {
	handler := newStockUnusualSSEHandler(&sequenceMonitorClient{}, testSSEConfig())
	market := types.MarketSZ.Uint8()
	handler.mu.Lock()
	handler.date = handler.now().In(shanghaiLocation).Format(time.DateOnly)
	handler.initialized[market] = true
	handler.mu.Unlock()

	other := testUnusualItem(1, "09:30:00", "别的股票")
	other.Code = "600000"
	handler.ingest(market, 0, []proto.MACMarketMonitorItem{
		testUnusualItem(1, "09:30:00", "自己"),
		other,
	}, true)

	subscriber, replay, _, _ := handler.subscribe(map[stockTarget]struct{}{
		{market: market, code: "000001"}: {},
	}, 0, false)
	defer handler.unsubscribe(subscriber)

	if len(replay) != 1 {
		t.Fatalf("首连补发必须按订阅股票过滤，got %d", len(replay))
	}
}

func TestStockUnusualSSEResetsWhenSinceTooOld(t *testing.T) {
	handler := newStockUnusualSSEHandler(&sequenceMonitorClient{}, testSSEConfig())
	market := types.MarketSZ.Uint8()
	handler.mu.Lock()
	handler.date = handler.now().In(shanghaiLocation).Format(time.DateOnly)
	handler.initialized[market] = true
	handler.mu.Unlock()
	startSeq := handler.CurrentSeq()
	handler.ingest(market, 0, []proto.MACMarketMonitorItem{
		testUnusualItem(1, "09:30:00", "第一"),
		testUnusualItem(2, "09:30:02", "第二"),
	}, true)

	subscriber, replay, resetSeq, _ := handler.subscribe(map[stockTarget]struct{}{
		{market: market, code: "000001"}: {},
	}, startSeq, true)
	defer handler.unsubscribe(subscriber)

	if resetSeq != startSeq+1 {
		t.Fatalf("since older than buffer must reset to base seq, got %d want %d", resetSeq, startSeq+1)
	}
	if len(replay) != 0 {
		t.Fatalf("reset must not replay stale events, got %d", len(replay))
	}
}

func TestStockUnusualSSECollectorRunsWithoutSubscribers(t *testing.T) {
	market := types.MarketSZ.Uint8()
	client := &sequenceMonitorClient{sequences: map[uint8][]monitorResult{
		market: {
			{items: nil},
			{items: []proto.MACMarketMonitorItem{testUnusualItem(1, "09:30:00", "常驻采集")}},
		},
	}}
	handler := newStockUnusualSSEHandler(client, stockUnusualSSEConfig{
		pollInterval:      10 * time.Millisecond,
		heartbeatInterval: time.Hour,
		autoStart:         true,
	})
	handler.now = tradingNow
	defer handler.Close()
	handler.Start()

	eventually(t, 2*time.Second, func() bool {
		return handler.CurrentSeq() > 0
	})
	handler.mu.Lock()
	subscribers := len(handler.subscribers)
	handler.mu.Unlock()
	if subscribers != 0 {
		t.Fatalf("collector must run without subscribers, got %d", subscribers)
	}
	handler.mu.Lock()
	failures := handler.failures
	handler.mu.Unlock()
	if failures != 0 {
		t.Fatalf("collector should stay healthy after successful polls, failures=%d", failures)
	}
}

func TestLunchBreakStopsPollingAfterBackfill(t *testing.T) {
	market := types.MarketSZ.Uint8()
	client := &sequenceMonitorClient{sequences: map[uint8][]monitorResult{
		market: {{items: nil}},
	}}
	handler := newStockUnusualSSEHandler(client, stockUnusualSSEConfig{
		pollInterval:      10 * time.Millisecond,
		heartbeatInterval: time.Hour,
	})
	handler.now = func() time.Time {
		return time.Date(2026, 11, 26, 12, 0, 0, 0, shanghaiLocation)
	}
	defer handler.Close()
	handler.Start()

	// 午休启动只做一次当天回填，之后应一直睡到 13:00。
	eventually(t, 2*time.Second, func() bool { return client.callCount(market) > 0 })
	first := client.callCount(market)
	time.Sleep(300 * time.Millisecond)
	if second := client.callCount(market); second != first {
		t.Fatalf("午休不应继续轮询: first=%d second=%d", first, second)
	}

	handler.mu.Lock()
	initialized := handler.initialized[market]
	handler.mu.Unlock()
	if !initialized {
		t.Fatal("午休启动也必须回填当天数据，否则午休打开 app 看不到上午的异动")
	}
}

func TestStockUnusualSSEErrorOnlyOnceAndRecovery(t *testing.T) {
	handler := newStockUnusualSSEHandler(&sequenceMonitorClient{}, testSSEConfig())
	subscriber := attachTestSubscriber(handler, map[stockTarget]struct{}{
		{market: types.MarketSZ.Uint8(), code: "000001"}: {},
	})
	defer handler.unsubscribe(subscriber)

	handler.handlePollError(errors.New("第一次失败"))
	handler.handlePollError(errors.New("重复失败"))
	if len(subscriber.messages) != 1 {
		t.Fatalf("same error should be sent once, got %d", len(subscriber.messages))
	}
	message := <-subscriber.messages
	if message.event != "error" || !json.Valid(message.data) {
		t.Fatalf("unexpected error event: %+v", message)
	}
	handler.mu.Lock()
	failuresAfterError := handler.failures
	handler.mu.Unlock()
	if failuresAfterError == 0 {
		t.Fatal("handler must record poll failures")
	}

	handler.markHealthy()
	handler.mu.Lock()
	failuresAfterRecovery := handler.failures
	handler.mu.Unlock()
	if failuresAfterRecovery != 0 {
		t.Fatalf("recovery must clear failures, got %d", failuresAfterRecovery)
	}

	handler.handlePollError(errors.New("恢复后再次失败"))
	if len(subscriber.messages) != 1 {
		t.Fatalf("error after recovery should be sent again, got %d", len(subscriber.messages))
	}
}

func TestStockUnusualSSEDailyResetAndDedup(t *testing.T) {
	handler := newStockUnusualSSEHandler(&sequenceMonitorClient{}, testSSEConfig())
	market := types.MarketSZ.Uint8()
	handler.now = func() time.Time {
		return time.Date(2026, 7, 21, 10, 0, 1, 0, shanghaiLocation)
	}
	subscriber := attachTestSubscriber(handler, map[stockTarget]struct{}{
		{market: market, code: "000001"}: {},
	})
	defer handler.unsubscribe(subscriber)

	oldItem := testUnusualItem(1, "09:30:00", "昨日同键异动")
	newItem := testUnusualItem(2, "09:31:00", "今日新增异动")

	handler.mu.Lock()
	handler.date = "2026-07-20"
	handler.initialized[market] = true
	handler.cursors[market] = 2
	handler.mu.Unlock()

	handler.ingest(market, 1, []proto.MACMarketMonitorItem{oldItem}, true)
	if len(subscriber.messages) != 1 {
		t.Fatalf("expected yesterday's event to be buffered, got %d", len(subscriber.messages))
	}
	<-subscriber.messages
	seqBefore := handler.CurrentSeq()

	// 跨日后缓冲与去重表清空，游标保持单调递增。
	handler.mu.Lock()
	handler.resetDayLocked(handler.now())
	buffered := len(handler.events)
	handler.mu.Unlock()
	if buffered != 0 {
		t.Fatalf("daily reset must clear buffer, got %d", buffered)
	}
	if handler.CurrentSeq() < seqBefore {
		t.Fatal("seq must stay monotonic across days")
	}

	// 跨日后同一键重新出现应被接受：上游序号每天重新从 0 开始。
	handler.ingest(market, 0, []proto.MACMarketMonitorItem{oldItem, newItem}, true)
	if len(subscriber.messages) != 2 {
		t.Fatalf("expected two events after daily reset, got %d", len(subscriber.messages))
	}
	<-subscriber.messages
	<-subscriber.messages

	// 同一批事件重复投递不应产生第三条。
	handler.ingest(market, 0, []proto.MACMarketMonitorItem{newItem}, true)
	if len(subscriber.messages) != 0 {
		t.Fatalf("duplicate event was sent again, got %d", len(subscriber.messages))
	}
}

func TestSeqStartsFromWallClockAndIncreasesAcrossRestarts(t *testing.T) {
	first := newStockUnusualSSEHandler(&sequenceMonitorClient{}, testSSEConfig())
	time.Sleep(2 * time.Millisecond)
	second := newStockUnusualSSEHandler(&sequenceMonitorClient{}, testSSEConfig())

	if first.CurrentSeq() == 0 {
		t.Fatal("seq must not start from zero")
	}
	if second.CurrentSeq() <= first.CurrentSeq() {
		t.Fatalf("restart must advance seq: first=%d second=%d", first.CurrentSeq(), second.CurrentSeq())
	}
	// 一天全市场两万余条，启动后的增量远小于起点，跨重启不可能追上。
	if second.CurrentSeq()-first.CurrentSeq() < 1_000_000 {
		t.Fatalf("seq base should be seconds-scale apart, got %d", second.CurrentSeq()-first.CurrentSeq())
	}
}

func TestIsTradingDayStarted(t *testing.T) {
	cases := []struct {
		name string
		when time.Time
		want bool
	}{
		{"工作日收盘后", time.Date(2026, 11, 26, 20, 0, 0, 0, shanghaiLocation), true},
		{"工作日收盘边界后一分钟", time.Date(2026, 11, 26, 15, 6, 0, 0, shanghaiLocation), true},
		{"工作日午休", time.Date(2026, 11, 26, 12, 0, 0, 0, shanghaiLocation), true},
		{"工作日开盘前一刻", time.Date(2026, 11, 26, 9, 14, 0, 0, shanghaiLocation), false},
		{"工作日凌晨", time.Date(2026, 11, 26, 0, 30, 0, 0, shanghaiLocation), false},
		{"周六收盘后时刻", time.Date(2026, 11, 28, 20, 0, 0, 0, shanghaiLocation), false},
		{"周日", time.Date(2026, 11, 29, 20, 0, 0, 0, shanghaiLocation), false},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			if got := isTradingDayStarted(item.when); got != item.want {
				t.Fatalf("want=%v got=%v for %s", item.want, got, item.when)
			}
		})
	}
}

func TestLunchBreakWindow(t *testing.T) {
	cases := []struct {
		name string
		when time.Time
		want bool
	}{
		{"上午收盘瞬间", time.Date(2026, 11, 26, 11, 30, 0, 0, shanghaiLocation), true},
		{"午休中段", time.Date(2026, 11, 26, 12, 0, 0, 0, shanghaiLocation), true},
		{"下午开盘前一刻", time.Date(2026, 11, 26, 12, 59, 0, 0, shanghaiLocation), true},
		{"下午开盘", time.Date(2026, 11, 26, 13, 0, 0, 0, shanghaiLocation), false},
		{"上午开盘", time.Date(2026, 11, 26, 9, 30, 0, 0, shanghaiLocation), false},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			if got := inStockUnusualLunchBreak(item.when); got != item.want {
				t.Fatalf("want=%v got=%v for %s", item.want, got, item.when)
			}
		})
	}
}

func TestLunchWaitSleepsUntilReopen(t *testing.T) {
	// 11:31 进入午休，应一直睡到 13:00（约 89 分钟），而不是几十秒就醒。
	now := time.Date(2026, 11, 26, 11, 31, 0, 0, shanghaiLocation)
	if wait := lunchWait(now); wait < 80*time.Minute {
		t.Fatalf("午休应睡到 13:00，got %v", wait)
	}
	// 12:59:30 只该睡 30 秒，保证 13:00 准时恢复采集。
	late := time.Date(2026, 11, 26, 12, 59, 30, 0, shanghaiLocation)
	if wait := lunchWait(late); wait != 30*time.Second {
		t.Fatalf("临近开盘应只睡到 13:00，got %v", wait)
	}
}

func TestBackfillIfNeededFillsBufferAndBroadcasts(t *testing.T) {
	client := &pagedMonitorClient{total: 100}
	handler := newStockUnusualSSEHandler(client, testSSEConfig())
	handler.now = func() time.Time {
		return time.Date(2026, 11, 26, 20, 0, 0, 0, shanghaiLocation)
	}
	subscriber := attachTestSubscriber(handler, map[stockTarget]struct{}{
		{market: types.MarketSZ.Uint8(), code: "000001"}: {},
	})
	defer handler.unsubscribe(subscriber)

	handler.backfillIfNeeded(handler.now().In(shanghaiLocation))

	handler.mu.Lock()
	buffered := len(handler.events)
	allInitialized := true
	for _, market := range stockUnusualMarkets {
		if !handler.initialized[market] {
			allInitialized = false
		}
	}
	handler.mu.Unlock()

	if buffered != 100*len(stockUnusualMarkets) {
		t.Fatalf("unexpected buffered events: %d", buffered)
	}
	if !allInitialized {
		t.Fatal("every market must be marked initialized after backfill")
	}
	// 只订阅深市这一只，因此只能收到深市那 100 条。
	if len(subscriber.messages) != 100 {
		t.Fatalf("only subscribed market should be broadcast, got %d", len(subscriber.messages))
	}
}

func TestBackfillSkippedOutsideAfterCloseWindow(t *testing.T) {
	client := &pagedMonitorClient{total: 320}
	handler := newStockUnusualSSEHandler(client, testSSEConfig())
	// 周六凌晨：上游返回的是周五数据，绝不能挂到当天日期上。
	handler.now = func() time.Time {
		return time.Date(2026, 11, 28, 0, 30, 0, 0, shanghaiLocation)
	}

	handler.backfillIfNeeded(handler.now().In(shanghaiLocation))

	handler.mu.Lock()
	buffered := len(handler.events)
	handler.mu.Unlock()
	if buffered != 0 {
		t.Fatalf("backfill must be skipped before close window, got %d", buffered)
	}
	if len(client.calls) != 0 {
		t.Fatalf("no upstream request should be made, got %d", len(client.calls))
	}
}

func TestBufferDatePersistedAndReloaded(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "unusual-buffer-date")

	friday := time.Date(2026, 11, 27, 15, 30, 0, 0, shanghaiLocation)
	first := newStockUnusualSSEHandler(&sequenceMonitorClient{}, testSSEConfig())
	first.datePath = path
	first.mu.Lock()
	first.resetDayLocked(friday)
	first.mu.Unlock()

	// 模拟进程重启：新实例从同一个文件读回交易日，周末打开 app 仍能正确标注。
	second := newStockUnusualSSEHandler(&sequenceMonitorClient{}, testSSEConfig())
	second.datePath = path
	second.date = loadBufferDate(path)
	if second.date != "2026-11-27" {
		t.Fatalf("重启后应读回上个交易日，got %q", second.date)
	}
}

func TestBackfillOnWeekendWhenBufferDateKnown(t *testing.T) {
	client := &pagedMonitorClient{total: 50}
	handler := newTestHandlerOnTempFile(t, client, testSSEConfig())
	// 上周五采过，周末重启：沿用这个标注回填上游当前保留的数据。
	handler.date = "2026-11-27"
	handler.now = func() time.Time {
		return time.Date(2026, 11, 28, 10, 0, 0, 0, shanghaiLocation)
	}

	handler.backfillIfNeeded(handler.now().In(shanghaiLocation))

	handler.mu.Lock()
	buffered := len(handler.events)
	gotDate := ""
	if buffered > 0 {
		gotDate = handler.events[0].Date
	}
	handler.mu.Unlock()

	if buffered != 50*len(stockUnusualMarkets) {
		t.Fatalf("周末重启应能用已知标注回填，got %d", buffered)
	}
	if gotDate != "2026-11-27" {
		t.Fatalf("回填事件必须沿用上次标注，got %q", gotDate)
	}
}

func TestBackfillSkippedOnWeekendWithoutKnownDate(t *testing.T) {
	client := &pagedMonitorClient{total: 50}
	handler := newTestHandlerOnTempFile(t, client, testSSEConfig())
	handler.date = ""
	handler.now = func() time.Time {
		return time.Date(2026, 11, 28, 10, 0, 0, 0, shanghaiLocation)
	}

	handler.backfillIfNeeded(handler.now().In(shanghaiLocation))

	handler.mu.Lock()
	buffered := len(handler.events)
	handler.mu.Unlock()
	if buffered != 0 {
		t.Fatalf("没有任何标注时不应猜测回填，got %d", buffered)
	}
}

func TestBufferSurvivesWeekendUntilNextTradingStart(t *testing.T) {
	handler := newStockUnusualSSEHandler(&sequenceMonitorClient{}, testSSEConfig())
	market := types.MarketSZ.Uint8()
	friday := time.Date(2026, 11, 27, 15, 30, 0, 0, shanghaiLocation)
	handler.mu.Lock()
	handler.resetDayLocked(friday)
	handler.mu.Unlock()
	handler.ingest(market, 0, []proto.MACMarketMonitorItem{
		testUnusualItem(1, "14:58:03", "周五尾盘"),
	}, true)

	buffered := func() int {
		handler.mu.Lock()
		defer handler.mu.Unlock()
		return len(handler.events)
	}
	eventDate := func() string {
		handler.mu.Lock()
		defer handler.mu.Unlock()
		if len(handler.events) == 0 {
			return ""
		}
		return handler.events[0].Date
	}

	if buffered() != 1 || eventDate() != "2026-11-27" {
		t.Fatalf("周五数据未入库: count=%d date=%s", buffered(), eventDate())
	}

	// 周六、周日、周一开盘前都不应清空：期间打开 app 仍要看到上一个交易日的全部异动。
	for _, when := range []time.Time{
		time.Date(2026, 11, 28, 10, 0, 0, 0, shanghaiLocation),
		time.Date(2026, 11, 29, 20, 0, 0, 0, shanghaiLocation),
		time.Date(2026, 11, 30, 9, 14, 0, 0, shanghaiLocation),
		time.Date(2026, 11, 30, 0, 30, 0, 0, shanghaiLocation),
	} {
		handler.mu.Lock()
		handler.resetDayLocked(when)
		handler.mu.Unlock()
		if buffered() != 1 {
			t.Fatalf("%s 不应清空上个交易日的缓冲，got %d", when.Format(time.DateTime), buffered())
		}
	}

	// 周一 09:15 开盘：切换到新交易日。
	mondayOpen := time.Date(2026, 11, 30, 9, 15, 0, 0, shanghaiLocation)
	handler.mu.Lock()
	handler.resetDayLocked(mondayOpen)
	handler.mu.Unlock()
	if buffered() != 0 {
		t.Fatalf("开盘应清空上个交易日的缓冲，got %d", buffered())
	}

	handler.ingest(market, 0, []proto.MACMarketMonitorItem{
		testUnusualItem(1, "09:15:03", "周一异动"),
	}, true)
	if eventDate() != "2026-11-30" {
		t.Fatalf("新事件应标注为周一，got %s", eventDate())
	}
}

func TestStockUnusualSSESlowSubscriberIsDisconnected(t *testing.T) {
	handler := newStockUnusualSSEHandler(&sequenceMonitorClient{}, testSSEConfig())
	market := types.MarketSZ.Uint8()
	subscriber := attachTestSubscriber(handler, map[stockTarget]struct{}{
		{market: market, code: "000001"}: {},
	})
	handler.mu.Lock()
	handler.date = handler.now().In(shanghaiLocation).Format(time.DateOnly)
	handler.initialized[market] = true
	handler.mu.Unlock()

	items := make([]proto.MACMarketMonitorItem, 0, stockUnusualSubscriberBuffer+1)
	for i := 0; i <= stockUnusualSubscriberBuffer; i++ {
		items = append(items, testUnusualItem(uint16(i+1), "09:30:00", fmt.Sprintf("异动%d", i)))
	}
	handler.ingest(market, 0, items, true)

	select {
	case <-subscriber.done:
	default:
		t.Fatal("slow subscriber was not disconnected")
	}
	handler.mu.Lock()
	defer handler.mu.Unlock()
	if len(handler.subscribers) != 0 {
		t.Fatalf("slow subscriber resources were not released")
	}
}

func TestStockUnusualSSEHeartbeat(t *testing.T) {
	client := &sequenceMonitorClient{sequences: map[uint8][]monitorResult{
		types.MarketSZ.Uint8(): {{items: nil}},
	}}
	handler := newStockUnusualSSEHandler(client, stockUnusualSSEConfig{
		pollInterval:      time.Hour,
		heartbeatInterval: 10 * time.Millisecond,
	})
	handler.now = tradingNow
	mux := http.NewServeMux()
	mux.Handle(StockUnusualSSEPath, handler)
	server := httptest.NewServer(mux)
	defer server.Close()
	defer handler.Close()

	response, cancel := openSSE(t, server.URL+StockUnusualSSEPath+"?stocks=000001")
	defer cancel()
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	if event := readSSEEvent(t, reader, time.Second); event.event != "ready" {
		t.Fatalf("first event must be ready, got %+v", event)
	}
	line := readLine(t, reader, time.Second)
	if line != ": ping\n" {
		t.Fatalf("unexpected heartbeat: %q", line)
	}
}

// tradingNow 把测试时钟固定在交易时段内，避免采集循环被时间窗口挡住。
func tradingNow() time.Time {
	return time.Date(2026, 11, 26, 10, 30, 0, 0, shanghaiLocation)
}

// newTestHandlerOnTempFile 让交易日标注落到临时目录，测试互不干扰也不污染工作区。
func newTestHandlerOnTempFile(t *testing.T, client MACMarketMonitorClient, config stockUnusualSSEConfig) *stockUnusualSSEHandler {
	t.Helper()
	handler := newStockUnusualSSEHandler(client, config)
	handler.datePath = filepath.Join(t.TempDir(), "unusual-buffer-date")
	handler.date = ""
	return handler
}

func testSSEConfig() stockUnusualSSEConfig {
	return stockUnusualSSEConfig{
		pollInterval:      time.Hour,
		heartbeatInterval: time.Hour,
		autoStart:         false,
	}
}

func testUnusualItem(index uint16, eventTime string, desc string) proto.MACMarketMonitorItem {
	return proto.MACMarketMonitorItem{
		Index:       index,
		Market:      uint16(types.MarketSZ),
		Code:        "000001",
		Name:        "平安银行",
		Time:        eventTime,
		Desc:        desc,
		Value:       "1.25%",
		UnusualType: 4,
		V1:          1,
		V2:          0.0125,
		V3:          2,
		V4:          3,
	}
}

func attachTestSubscriber(handler *stockUnusualSSEHandler, targets map[stockTarget]struct{}) *stockUnusualSubscriber {
	subscriber := &stockUnusualSubscriber{
		targets:  targets,
		messages: make(chan sseMessage, stockUnusualSubscriberBuffer),
		done:     make(chan struct{}),
	}
	handler.mu.Lock()
	handler.subscribers[subscriber] = struct{}{}
	handler.mu.Unlock()
	return subscriber
}

func openSSE(t *testing.T, url string) (*http.Response, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		cancel()
		t.Fatalf("open SSE failed: %v", err)
	}
	return response, cancel
}

type parsedSSEEvent struct {
	event string
	data  []byte
}

func readSSEEvent(t *testing.T, reader *bufio.Reader, timeout time.Duration) parsedSSEEvent {
	t.Helper()
	type result struct {
		event parsedSSEEvent
		err   error
	}
	resultCh := make(chan result, 1)
	go func() {
		var event parsedSSEEvent
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				resultCh <- result{err: err}
				return
			}
			line = strings.TrimSuffix(line, "\n")
			line = strings.TrimSuffix(line, "\r")
			if line == "" {
				if event.event != "" {
					resultCh <- result{event: event}
					return
				}
				continue
			}
			if strings.HasPrefix(line, "event: ") {
				event.event = strings.TrimPrefix(line, "event: ")
			}
			if strings.HasPrefix(line, "data: ") {
				event.data = []byte(strings.TrimPrefix(line, "data: "))
			}
		}
	}()

	select {
	case got := <-resultCh:
		if got.err != nil {
			t.Fatalf("read SSE event failed: %v", got.err)
		}
		return got.event
	case <-time.After(timeout):
		t.Fatal("timed out waiting for SSE event")
		return parsedSSEEvent{}
	}
}

func readLine(t *testing.T, reader *bufio.Reader, timeout time.Duration) string {
	t.Helper()
	type result struct {
		line string
		err  error
	}
	resultCh := make(chan result, 1)
	go func() {
		line, err := reader.ReadString('\n')
		resultCh <- result{line: line, err: err}
	}()
	select {
	case got := <-resultCh:
		if got.err != nil && !errors.Is(got.err, io.EOF) {
			t.Fatalf("read line failed: %v", got.err)
		}
		return got.line
	case <-time.After(timeout):
		t.Fatal("timed out waiting for SSE line")
		return ""
	}
}

func eventually(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition was not met before timeout")
}
