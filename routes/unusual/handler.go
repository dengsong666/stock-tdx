// Package unusual 提供批量个股异动 SSE 服务。
package unusual

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bensema/gotdx/proto"
	"github.com/bensema/gotdx/types"
)

const (
	// StockUnusualSSEPath 是批量个股异动 SSE 接口的固定路径。
	StockUnusualSSEPath = "/api/stock/unusual/sse"

	defaultStockUnusualPollInterval      = 2 * time.Second
	defaultStockUnusualHeartbeatInterval = time.Minute
	stockUnusualMonitorCount             = uint32(600)
	stockUnusualMaxStart                 = uint32(^uint16(0))
	stockUnusualMaxStocks                = 100
	stockUnusualSubscriberBuffer         = 128
	stockUnusualPollAttempts             = 3
)

var shanghaiLocation = time.FixedZone("Asia/Shanghai", 8*60*60)

// MACMarketMonitorClient 描述 SSE 路由需要的行情查询能力。
// *gotdx.Client 已经实现这个接口，测试也可以传入假的行情客户端。
type MACMarketMonitorClient interface {
	MACMarketMonitor(market uint8, start uint32, count uint32) ([]proto.MACMarketMonitorItem, error)
}

type macMarketMonitorDisconnector interface {
	Disconnect() error
}

// StockUnusualSSEOption 用于修改 SSE 路由的可选配置。
type StockUnusualSSEOption func(*stockUnusualSSEConfig)

type stockUnusualSSEConfig struct {
	pollInterval      time.Duration
	heartbeatInterval time.Duration
}

// WithPollInterval 修改通达信异动接口的轮询间隔。
// 小于或等于零的值会被忽略。
func WithPollInterval(interval time.Duration) StockUnusualSSEOption {
	return func(config *stockUnusualSSEConfig) {
		if interval > 0 {
			config.pollInterval = interval
		}
	}
}

// WithHeartbeatInterval 修改 SSE 注释心跳间隔。
// 小于或等于零的值会被忽略。
func WithHeartbeatInterval(interval time.Duration) StockUnusualSSEOption {
	return func(config *stockUnusualSSEConfig) {
		if interval > 0 {
			config.heartbeatInterval = interval
		}
	}
}

// RegisterStockUnusualSSE 在 mux 上注册批量个股异动 SSE 接口。
func RegisterStockUnusualSSE(mux *http.ServeMux, client MACMarketMonitorClient, options ...StockUnusualSSEOption) {
	config := stockUnusualSSEConfig{
		pollInterval:      defaultStockUnusualPollInterval,
		heartbeatInterval: defaultStockUnusualHeartbeatInterval,
	}
	for _, option := range options {
		if option != nil {
			option(&config)
		}
	}

	handler := newStockUnusualSSEHandler(client, config)
	mux.Handle(StockUnusualSSEPath, handler)
}

type stockTarget struct {
	market uint8
	code   string
}

type unusualEventKey struct {
	market uint8
	code   string
	index  uint16
	time   string
	typeID uint8
}

type unusualRawPayload struct {
	V1 uint8   `json:"v1"`
	V2 float64 `json:"v2"`
	V3 float64 `json:"v3"`
	V4 float64 `json:"v4"`
}

type unusualEventPayload struct {
	Code  string            `json:"code"`
	Name  string            `json:"name"`
	Time  string            `json:"time"`
	Desc  string            `json:"desc"`
	Value string            `json:"value"`
	Type  uint8             `json:"type"`
	Raw   unusualRawPayload `json:"raw"`
}

type sseMessage struct {
	event string
	data  []byte
}

type stockUnusualSubscriber struct {
	targets       map[stockTarget]struct{}
	markets       map[uint8]struct{}
	failedMarkets map[uint8]struct{}
	messages      chan sseMessage
	done          chan struct{}
}

type stockUnusualMarketState struct {
	date        string
	initialized bool
	inError     bool
	nextStart   uint32
	seen        map[unusualEventKey]struct{}
}

type stockUnusualSSEHandler struct {
	client MACMarketMonitorClient
	config stockUnusualSSEConfig
	now    func() time.Time

	mu          sync.Mutex
	subscribers map[*stockUnusualSubscriber]struct{}
	marketRefs  map[uint8]int
	states      map[uint8]*stockUnusualMarketState
	running     bool
	wake        chan struct{}
}

func newStockUnusualSSEHandler(client MACMarketMonitorClient, config stockUnusualSSEConfig) *stockUnusualSSEHandler {
	return &stockUnusualSSEHandler{
		client:      client,
		config:      config,
		now:         time.Now,
		subscribers: make(map[*stockUnusualSubscriber]struct{}),
		marketRefs:  make(map[uint8]int),
		states:      make(map[uint8]*stockUnusualMarketState),
		wake:        make(chan struct{}, 1),
	}
}

func (handler *stockUnusualSSEHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeRouteJSONError(w, http.StatusMethodNotAllowed, "请求方法不允许")
		return
	}

	targets, err := parseStockTargets(r.URL.Query().Get("stocks"))
	if err != nil {
		writeRouteJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if handler.client == nil {
		writeRouteJSONError(w, http.StatusServiceUnavailable, "MAC 行情客户端不可用")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeRouteJSONError(w, http.StatusInternalServerError, "当前 HTTP 服务不支持 SSE")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	subscriber := handler.subscribe(targets)
	defer handler.unsubscribe(subscriber)

	heartbeat := time.NewTicker(handler.config.heartbeatInterval)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-subscriber.done:
			return
		case message := <-subscriber.messages:
			if err := writeSSEMessage(w, message); err != nil {
				return
			}
			flusher.Flush()
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func parseStockTargets(raw string) (map[stockTarget]struct{}, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("stocks 参数不能为空")
	}

	targets := make(map[stockTarget]struct{})
	for _, part := range strings.Split(raw, ",") {
		code := strings.TrimSpace(part)
		if code == "" {
			return nil, fmt.Errorf("stocks 参数包含空股票代码")
		}
		if len(code) != 6 || !isASCIIDigits(code) {
			return nil, fmt.Errorf("股票代码必须是 6 位数字: %s", code)
		}

		market, normalizedCode, err := types.DecodeStockCode(code)
		if err != nil || !types.IsStock(normalizedCode+"."+market.String()) {
			return nil, fmt.Errorf("无法识别股票市场: %s", code)
		}
		targets[stockTarget{market: market.Uint8(), code: normalizedCode}] = struct{}{}
	}

	if len(targets) > stockUnusualMaxStocks {
		return nil, fmt.Errorf("最多只能订阅 %d 只股票", stockUnusualMaxStocks)
	}
	return targets, nil
}

func isASCIIDigits(value string) bool {
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func (handler *stockUnusualSSEHandler) subscribe(targets map[stockTarget]struct{}) *stockUnusualSubscriber {
	subscriber := &stockUnusualSubscriber{
		targets:       targets,
		markets:       make(map[uint8]struct{}),
		failedMarkets: make(map[uint8]struct{}),
		messages:      make(chan sseMessage, stockUnusualSubscriberBuffer),
		done:          make(chan struct{}),
	}
	for target := range targets {
		subscriber.markets[target.market] = struct{}{}
	}

	handler.mu.Lock()
	handler.subscribers[subscriber] = struct{}{}
	newMarket := false
	for market := range subscriber.markets {
		if handler.marketRefs[market] == 0 {
			newMarket = true
			handler.states[market] = &stockUnusualMarketState{seen: make(map[unusualEventKey]struct{})}
		}
		handler.marketRefs[market]++
		if state := handler.states[market]; state != nil && state.inError {
			subscriber.failedMarkets[market] = struct{}{}
		}
	}
	if len(subscriber.failedMarkets) > 0 {
		subscriber.messages <- stockUnusualErrorMessage()
	}

	start := !handler.running
	if start {
		handler.running = true
	}
	handler.mu.Unlock()

	if start {
		go handler.runPollLoop()
	} else if newMarket {
		handler.notifyPollLoop()
	}
	return subscriber
}

func (handler *stockUnusualSSEHandler) unsubscribe(subscriber *stockUnusualSubscriber) {
	handler.mu.Lock()
	handler.removeSubscriberLocked(subscriber)
	handler.mu.Unlock()
}

func (handler *stockUnusualSSEHandler) removeSubscriberLocked(subscriber *stockUnusualSubscriber) {
	if _, ok := handler.subscribers[subscriber]; !ok {
		return
	}
	delete(handler.subscribers, subscriber)
	close(subscriber.done)

	removedMarket := false
	for market := range subscriber.markets {
		handler.marketRefs[market]--
		if handler.marketRefs[market] <= 0 {
			delete(handler.marketRefs, market)
			delete(handler.states, market)
			removedMarket = true
		}
	}
	if removedMarket || len(handler.subscribers) == 0 {
		handler.notifyPollLoopLocked()
	}
}

func (handler *stockUnusualSSEHandler) runPollLoop() {
	for {
		markets, ok := handler.activeMarkets()
		if !ok {
			return
		}
		for _, market := range markets {
			handler.pollMarket(market)
		}

		timer := time.NewTimer(handler.config.pollInterval)
		select {
		case <-timer.C:
		case <-handler.wake:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}
	}
}

func (handler *stockUnusualSSEHandler) pollMarket(market uint8) {
	today := handler.now().In(shanghaiLocation).Format(time.DateOnly)

	handler.mu.Lock()
	state := handler.states[market]
	if state == nil {
		handler.mu.Unlock()
		return
	}
	needsBaseline := !state.initialized || state.date != today
	start := state.nextStart
	handler.mu.Unlock()

	if needsBaseline {
		nextStart, err := handler.findMarketTail(market)
		if err != nil {
			handler.handlePollError(market, err)
			return
		}
		handler.handleBaseline(market, today, nextStart)
		return
	}

	for {
		items, err := handler.queryMarketMonitor(market, start, stockUnusualMonitorCount)
		if err != nil {
			handler.handlePollError(market, err)
			return
		}
		if !handler.handlePollItems(market, today, start, items) {
			return
		}
		if len(items) < int(stockUnusualMonitorCount) {
			return
		}
		start += uint32(len(items))
	}
}

func (handler *stockUnusualSSEHandler) queryMarketMonitor(market uint8, start uint32, count uint32) ([]proto.MACMarketMonitorItem, error) {
	if handler.client == nil {
		return nil, fmt.Errorf("MAC 行情客户端不可用")
	}
	var lastErr error
	for attempt := 1; attempt <= stockUnusualPollAttempts; attempt++ {
		items, err := handler.client.MACMarketMonitor(market, start, count)
		if disconnector, ok := handler.client.(macMarketMonitorDisconnector); ok {
			_ = disconnector.Disconnect()
		}
		if err == nil {
			return items, nil
		}
		lastErr = err
		log.Printf(
			"stock unusual query failed: market=%d start=%d count=%d attempt=%d/%d: %v",
			market,
			start,
			count,
			attempt,
			stockUnusualPollAttempts,
			err,
		)
	}
	return nil, fmt.Errorf(
		"market=%d start=%d count=%d failed after %d attempts: %w",
		market,
		start,
		count,
		stockUnusualPollAttempts,
		lastErr,
	)
}

func (handler *stockUnusualSSEHandler) findMarketTail(market uint8) (uint32, error) {
	pageSize := stockUnusualMonitorCount
	pageLengths := make(map[uint32]int)
	queryPage := func(page uint32) (int, error) {
		if length, ok := pageLengths[page]; ok {
			return length, nil
		}
		start := page * pageSize
		if start > stockUnusualMaxStart {
			return 0, fmt.Errorf("市场异动游标超过协议上限: %d", start)
		}
		items, err := handler.queryMarketMonitor(market, start, pageSize)
		if err != nil {
			return 0, err
		}
		pageLengths[page] = len(items)
		return len(items), nil
	}

	firstLength, err := queryPage(0)
	if err != nil {
		return 0, err
	}
	if firstLength < int(pageSize) {
		return uint32(firstLength), nil
	}

	lowPage := uint32(0)
	highPage := uint32(1)
	maxPage := stockUnusualMaxStart / pageSize
	for {
		if highPage > maxPage {
			highPage = maxPage
		}
		length, err := queryPage(highPage)
		if err != nil {
			return 0, err
		}
		if length < int(pageSize) {
			break
		}
		if highPage == maxPage {
			return 0, fmt.Errorf("市场异动数量达到协议游标上限")
		}
		lowPage = highPage
		highPage *= 2
	}

	left := lowPage + 1
	right := highPage
	for left < right {
		middle := left + (right-left)/2
		length, err := queryPage(middle)
		if err != nil {
			return 0, err
		}
		if length == int(pageSize) {
			left = middle + 1
		} else {
			right = middle
		}
	}

	tailLength, err := queryPage(left)
	if err != nil {
		return 0, err
	}
	return left*pageSize + uint32(tailLength), nil
}

func (handler *stockUnusualSSEHandler) activeMarkets() ([]uint8, bool) {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	if len(handler.subscribers) == 0 {
		handler.running = false
		select {
		case <-handler.wake:
		default:
		}
		return nil, false
	}

	markets := make([]uint8, 0, len(handler.marketRefs))
	for market := range handler.marketRefs {
		markets = append(markets, market)
	}
	sort.Slice(markets, func(i, j int) bool { return markets[i] < markets[j] })
	return markets, true
}

func (handler *stockUnusualSSEHandler) handlePollError(market uint8, pollErr error) {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	state := handler.states[market]
	if state == nil {
		return
	}
	if state.inError {
		return
	}
	state.inError = true
	log.Printf("stock unusual poll failed: %v", pollErr)

	message := stockUnusualErrorMessage()
	for subscriber := range handler.subscribers {
		if _, watches := subscriber.markets[market]; !watches {
			continue
		}
		wasHealthy := len(subscriber.failedMarkets) == 0
		subscriber.failedMarkets[market] = struct{}{}
		if wasHealthy {
			handler.enqueueLocked(subscriber, message)
		}
	}
}

func (handler *stockUnusualSSEHandler) handleBaseline(market uint8, today string, nextStart uint32) {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	state := handler.states[market]
	if state == nil {
		return
	}
	state.date = today
	state.initialized = true
	state.nextStart = nextStart
	state.seen = make(map[unusualEventKey]struct{})
	if state.inError {
		state.inError = false
		for subscriber := range handler.subscribers {
			delete(subscriber.failedMarkets, market)
		}
	}
}

func (handler *stockUnusualSSEHandler) handlePollItems(market uint8, today string, start uint32, items []proto.MACMarketMonitorItem) bool {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	state := handler.states[market]
	if state == nil || !state.initialized || state.date != today || state.nextStart != start {
		return false
	}
	if state.inError {
		state.inError = false
		for subscriber := range handler.subscribers {
			delete(subscriber.failedMarkets, market)
		}
	}

	nextStart := start + uint32(len(items))
	for _, item := range items {
		itemNextStart := uint32(item.Index) + 1
		if itemNextStart > nextStart {
			nextStart = itemNextStart
		}
	}
	state.nextStart = nextStart

	for _, item := range items {
		key := makeUnusualEventKey(market, item)
		if _, seen := state.seen[key]; seen {
			continue
		}
		state.seen[key] = struct{}{}

		data, err := json.Marshal(makeUnusualEventPayload(item))
		if err != nil {
			continue
		}
		message := sseMessage{event: "unusual", data: data}
		target := stockTarget{market: market, code: item.Code}
		for subscriber := range handler.subscribers {
			if _, watches := subscriber.targets[target]; !watches {
				continue
			}
			handler.enqueueLocked(subscriber, message)
		}
	}
	return true
}

func makeUnusualEventKey(market uint8, item proto.MACMarketMonitorItem) unusualEventKey {
	return unusualEventKey{
		market: market,
		code:   item.Code,
		index:  item.Index,
		time:   item.Time,
		typeID: item.UnusualType,
	}
}

func makeUnusualEventPayload(item proto.MACMarketMonitorItem) unusualEventPayload {
	return unusualEventPayload{
		Code:  item.Code,
		Name:  item.Name,
		Time:  item.Time,
		Desc:  item.Desc,
		Value: item.Value,
		Type:  item.UnusualType,
		Raw: unusualRawPayload{
			V1: item.V1,
			V2: finiteJSONFloat(item.V2),
			V3: finiteJSONFloat(item.V3),
			V4: finiteJSONFloat(item.V4),
		},
	}
}

func finiteJSONFloat(value float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0
	}
	return value
}

func (handler *stockUnusualSSEHandler) enqueueLocked(subscriber *stockUnusualSubscriber, message sseMessage) {
	select {
	case subscriber.messages <- message:
	default:
		handler.removeSubscriberLocked(subscriber)
	}
}

func (handler *stockUnusualSSEHandler) notifyPollLoop() {
	handler.mu.Lock()
	handler.notifyPollLoopLocked()
	handler.mu.Unlock()
}

func (handler *stockUnusualSSEHandler) notifyPollLoopLocked() {
	select {
	case handler.wake <- struct{}{}:
	default:
	}
}

func stockUnusualErrorMessage() sseMessage {
	return sseMessage{event: "error", data: []byte(`{"message":"MAC 行情轮询失败"}`)}
}

func writeSSEMessage(w http.ResponseWriter, message sseMessage) error {
	if _, err := fmt.Fprintf(w, "event: %s\n", message.event); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", message.data); err != nil {
		return err
	}
	return nil
}

func writeRouteJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}
