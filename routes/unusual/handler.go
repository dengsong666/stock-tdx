// Package unusual 提供批量个股异动 SSE 服务。
//
// 服务内部把「采集」与「推送」拆成两层：
// 采集层在交易时段常驻轮询通达信 MAC 市场异动，把事件写入当天内存缓冲并分配单调递增游标；
// 推送层只在有订阅者时把新事件 fan-out 出去，并支持按游标补发断线期间漏掉的事件。
// 采集不依赖订阅者，因此手机端掉线期间的事件不会丢失，重连后可补齐。
package unusual

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
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
	stockUnusualMaxStocks                = 500
	stockUnusualSubscriberBuffer         = 512
	stockUnusualPollAttempts             = 3
	// stockUnusualEventBufferLimit 是当天事件缓冲上限；一天全市场约两万条，留足余量后按 FIFO 淘汰最旧事件。
	stockUnusualEventBufferLimit = 60000
	// stockUnusualTradingStartMinute 与 stockUnusualTradingEndMinute 是采集窗口边界（含首尾），单位是当天第几分钟。
	stockUnusualTradingStartMinute = 9*60 + 15
	stockUnusualTradingEndMinute   = 15*60 + 5
	stockUnusualLunchStartMinute   = 11*60 + 30
	stockUnusualLunchEndMinute     = 13 * 60
	// stockUnusualIdleWait 是非交易时段采集协程的休眠上限，保证次日开盘能被及时唤醒。
	stockUnusualIdleWait = time.Minute
	// unusualBufferDateEnv 可覆盖缓冲交易日标注的落盘路径，测试与本地运行时使用。
	unusualBufferDateEnv = "TDX_UNUSUAL_DATE_FILE"
)

var (
	shanghaiLocation = time.FixedZone("Asia/Shanghai", 8*60*60)
	// stockUnusualMarkets 是常驻采集覆盖的市场：深市、沪市、北交所。
	stockUnusualMarkets = []uint8{
		types.MarketSZ.Uint8(),
		types.MarketSH.Uint8(),
		types.MarketBJ.Uint8(),
	}
)

// MACMarketMonitorClient 描述 SSE 路由需要的行情查询能力。
// *gotdx.Client 已经实现这个接口，测试也可以传入假的行情客户端。
type MACMarketMonitorClient interface {
	MACMarketMonitor(market uint8, start uint32, count uint32) ([]proto.MACMarketMonitorItem, error)
}

// StockUnusualSSEOption 用于修改 SSE 路由的可选配置。
type StockUnusualSSEOption func(*stockUnusualSSEConfig)

type stockUnusualSSEConfig struct {
	pollInterval      time.Duration
	heartbeatInterval time.Duration
	autoStart         bool
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

// WithCollectorAutoStart 控制注册后是否立即启动常驻采集。
// 默认开启；离线测试需要自己控制采集节奏时传 false，再手动调用 Start。
func WithCollectorAutoStart(enabled bool) StockUnusualSSEOption {
	return func(config *stockUnusualSSEConfig) {
		config.autoStart = enabled
	}
}

// StockUnusualService 是异动 SSE 路由的句柄，供调用方控制常驻采集的生命周期。
type StockUnusualService = stockUnusualSSEHandler

// RegisterStockUnusualSSE 在 mux 上注册批量个股异动 SSE 接口。
func RegisterStockUnusualSSE(mux *http.ServeMux, client MACMarketMonitorClient, options ...StockUnusualSSEOption) *StockUnusualService {
	config := stockUnusualSSEConfig{
		pollInterval:      defaultStockUnusualPollInterval,
		heartbeatInterval: defaultStockUnusualHeartbeatInterval,
		autoStart:         true,
	}
	for _, option := range options {
		if option != nil {
			option(&config)
		}
	}

	handler := newStockUnusualSSEHandler(client, config)
	// 交易日标注只在正式注册时落盘：直接构造（测试）不读写磁盘，避免相互串扰。
	handler.datePath = defaultBufferDatePath()
	handler.date = loadBufferDate(handler.datePath)
	mux.Handle(StockUnusualSSEPath, handler)
	if config.autoStart {
		handler.Start()
	}
	return handler
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

// unusualStoredEvent 是缓冲中的一条事件，Seq 是全局单调递增游标，跨日不清零。
type unusualStoredEvent struct {
	Seq    uint64
	Market uint8
	// Date 是这条事件所属的交易日，客户端用它作为日期标注；缓冲跨周末保留时不会错标成打开当天。
	Date string
	Item proto.MACMarketMonitorItem
}

type unusualRawPayload struct {
	V1 uint8   `json:"v1"`
	V2 float64 `json:"v2"`
	V3 float64 `json:"v3"`
	V4 float64 `json:"v4"`
}

type unusualEventPayload struct {
	Seq   uint64 `json:"seq"`
	Date  string `json:"date"`
	Code  string `json:"code"`
	Name  string `json:"name"`
	Time  string `json:"time"`
	Desc  string `json:"desc"`
	Value string `json:"value"`
	// Amount 金额（元）：只有主力买卖、大单托盘/压盘、竞价试买、涨跌停组才有；
	// 尾盘组与纯百分比类不带，字段省略。客户端按「万 / 亿」展示与播报。
	Amount *float64          `json:"amount,omitempty"`
	Type   uint8             `json:"type"`
	Raw    unusualRawPayload `json:"raw"`
}

type sseMessage struct {
	event string
	data  []byte
}

type stockUnusualSubscriber struct {
	targets  map[stockTarget]struct{}
	messages chan sseMessage
	done     chan struct{}
}

type stockUnusualSSEHandler struct {
	client MACMarketMonitorClient
	config stockUnusualSSEConfig
	now    func() time.Time

	mu          sync.Mutex
	date        string
	datePath    string
	seq         uint64
	events      []unusualStoredEvent
	seen        map[unusualEventKey]struct{}
	initialized map[uint8]bool
	cursors     map[uint8]uint32
	subscribers map[*stockUnusualSubscriber]struct{}
	failures    int
	lastPollAt  time.Time
	lastError   string
	running     bool
	closed      bool
	stop        chan struct{}
}

func newStockUnusualSSEHandler(client MACMarketMonitorClient, config stockUnusualSSEConfig) *stockUnusualSSEHandler {
	return &stockUnusualSSEHandler{
		client:      client,
		config:      config,
		now:         time.Now,
		seen:        make(map[unusualEventKey]struct{}),
		initialized: make(map[uint8]bool),
		cursors:     make(map[uint8]uint32),
		subscribers: make(map[*stockUnusualSubscriber]struct{}),
		stop:        make(chan struct{}),
		// 游标起点取启动时刻的毫秒时间戳：跨进程重启必须单调递增。若每次从 1 开始，
		// 客户端存的 since 会落在新游标空间的中间——服务端既不会发 reset，又会补发一批
		// 与旧游标含义完全无关的事件，客户端静默漏数据且无从察觉。
		seq: uint64(time.Now().UnixMilli()) * 1_000_000,
	}
}

// Start 启动常驻采集协程；重复调用无副作用。
func (handler *stockUnusualSSEHandler) Start() {
	handler.mu.Lock()
	if handler.running || handler.closed || handler.client == nil {
		handler.mu.Unlock()
		return
	}
	handler.running = true
	handler.mu.Unlock()
	go handler.runCollector()
}

// Close 停止常驻采集并断开所有订阅者，供测试与进程退出使用。
func (handler *stockUnusualSSEHandler) Close() {
	handler.mu.Lock()
	if handler.closed {
		handler.mu.Unlock()
		return
	}
	handler.closed = true
	handler.running = false
	close(handler.stop)
	for subscriber := range handler.subscribers {
		delete(handler.subscribers, subscriber)
		close(subscriber.done)
	}
	handler.mu.Unlock()
}

// defaultBufferDatePath 返回缓冲交易日标注的落盘路径；容器内落在 /app/data 下随卷持久化。
func defaultBufferDatePath() string {
	if custom := os.Getenv(unusualBufferDateEnv); custom != "" {
		return custom
	}
	return filepath.Join("data", "unusual-buffer-date")
}

// loadBufferDate 读回上一次采集所属的交易日；读不到或不合法时返回空，调用方据此不做猜测。
func loadBufferDate(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	value := strings.TrimSpace(string(raw))
	if len(value) != len("2006-01-02") {
		return ""
	}
	return value
}

// persistBufferDate 记录当前缓冲所属的交易日。只为服务重启后沿用同一标注，
// 不保存任何事件内容，写失败不影响主流程。
func (handler *stockUnusualSSEHandler) persistBufferDate(date string) {
	if handler.datePath == "" || date == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(handler.datePath), 0o755); err != nil {
		log.Printf("stock unusual buffer date mkdir failed: %v", err)
		return
	}
	if err := os.WriteFile(handler.datePath, []byte(date), 0o644); err != nil {
		log.Printf("stock unusual buffer date write failed: %v", err)
	}
}

// BaseSeq 返回当前缓冲中最早事件的游标，缓冲为空时等于已分配的最大游标。
func (handler *stockUnusualSSEHandler) BaseSeq() uint64 {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	return handler.baseSeqLocked()
}

// CurrentSeq 返回已分配的最大游标，客户端用它作为下次重连的 since。
func (handler *stockUnusualSSEHandler) CurrentSeq() uint64 {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	return handler.seq
}

func (handler *stockUnusualSSEHandler) baseSeqLocked() uint64 {
	if len(handler.events) == 0 {
		return handler.seq
	}
	return handler.events[0].Seq
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
	since, hasSince, err := parseUnusualSince(r.URL.Query().Get("since"))
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

	handler.Start()

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	subscriber, replay, resetSeq, readySeq := handler.subscribe(targets, since, hasSince)
	defer handler.unsubscribe(subscriber)

	// 断线重连时先补发缓冲中的历史事件，再进入实时转发；补发不占用订阅通道，避免瞬间撑满缓冲。
	if resetSeq > 0 {
		if err := writeSSEMessage(w, resetMessage(resetSeq)); err != nil {
			return
		}
	}
	if err := writeSSEMessage(w, readyMessage(readySeq)); err != nil {
		return
	}
	for _, message := range replay {
		if err := writeSSEMessage(w, message); err != nil {
			return
		}
	}
	flusher.Flush()

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

func parseUnusualSince(raw string) (uint64, bool, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return 0, false, nil
	}
	seq, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("since 必须是非负整数游标: %s", raw)
	}
	return seq, true, nil
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

// subscribe 注册订阅者并返回需要补发的历史事件。
//
// 入参 since 表示客户端已收到的最大游标：不带 since 时补发缓冲里的当天全量；
// 带 since 且落在缓冲范围内时补发 (since, 当前游标] 的增量；早于缓冲起点时
// 通过 resetSeq 要求客户端清空本地缓存重新对齐。
func (handler *stockUnusualSSEHandler) subscribe(targets map[stockTarget]struct{}, since uint64, hasSince bool) (*stockUnusualSubscriber, []sseMessage, uint64, uint64) {
	subscriber := &stockUnusualSubscriber{
		targets:  targets,
		messages: make(chan sseMessage, stockUnusualSubscriberBuffer),
		done:     make(chan struct{}),
	}

	handler.mu.Lock()
	defer handler.mu.Unlock()

	handler.subscribers[subscriber] = struct{}{}

	baseSeq := handler.baseSeqLocked()
	readySeq := handler.seq

	// 首连也从缓冲起点补发：app 可能是下午或收盘后才第一次打开，只从尾部开始会看不到
	// 当天已经发生的异动。带 since 时按增量补发；早于缓冲起点说明客户端游标已失效，
	// 返回 resetSeq 让它清空本地游标重新对齐。
	after := uint64(0)
	if baseSeq > 0 {
		after = baseSeq - 1
	}
	if hasSince {
		if since < baseSeq {
			return subscriber, nil, baseSeq, readySeq
		}
		after = since
	}

	replay := make([]sseMessage, 0, 64)
	for _, event := range handler.events {
		if event.Seq <= after {
			continue
		}
		target := stockTarget{market: event.Market, code: event.Item.Code}
		if _, watches := subscriber.targets[target]; !watches {
			continue
		}
		data, err := json.Marshal(makeUnusualEventPayload(event))
		if err != nil {
			continue
		}
		replay = append(replay, sseMessage{event: "unusual", data: data})
	}
	return subscriber, replay, 0, readySeq
}

func (handler *stockUnusualSSEHandler) unsubscribe(subscriber *stockUnusualSubscriber) {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	if _, ok := handler.subscribers[subscriber]; !ok {
		return
	}
	delete(handler.subscribers, subscriber)
	close(subscriber.done)
}

// runCollector 是常驻采集循环：交易时段按固定间隔轮询，非交易时段休眠等待。
func (handler *stockUnusualSSEHandler) runCollector() {
	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		select {
		case <-handler.stop:
			return
		case <-timer.C:
		}

		now := handler.now().In(shanghaiLocation)
		if !inStockUnusualTradingWindow(now) {
			// 收盘后做一次当天回填，晚上部署重启后当天事件仍能在缓冲里供客户端补发。
			// 周末与次日凌晨刻意不做：那时上游返回的是上一个交易日的数据，挂到当天日期上
			// 会让次日开盘的游标从上一个交易日的尾部开始，永远拉不到新事件。
			handler.backfillIfNeeded(now)
			timer.Reset(handler.idleWait(now))
			continue
		}

		if inStockUnusualLunchBreak(now) {
			// 午休不采集：没有交易就没有新异动。游标停在 11:30 的位置，13:00 恢复后
			// 一次增量请求就能把午休期间延迟写入的事件全部补齐，不会漏。
			// 缓冲若为空（午休重启服务）先补一次当天数据，保证午休打开 app 也能看到上午。
			handler.backfillIfNeeded(now)
			timer.Reset(lunchWait(now))
			continue
		}

		for _, market := range stockUnusualMarkets {
			handler.pollMarket(market, now)
		}
		timer.Reset(handler.config.pollInterval)
	}
}

// lunchWait 返回午休期间的休眠时长：直接睡到 13:00 恢复，中途不做任何采集。
func lunchWait(now time.Time) time.Duration {
	reopen := time.Date(now.Year(), now.Month(), now.Day(), 13, 0, 0, 0, shanghaiLocation)
	if wait := reopen.Sub(now); wait > 0 {
		return wait
	}
	return time.Second
}

// inStockUnusualLunchBreak 判断是否处于午休：11:30（含）到 13:00（不含）。
func inStockUnusualLunchBreak(t time.Time) bool {
	minute := t.Hour()*60 + t.Minute()
	return minute >= stockUnusualLunchStartMinute && minute < stockUnusualLunchEndMinute
}

// isTradingDayStarted 判断是否已进入某个交易日的 09:15（工作日，含午休与收盘后）。
//
// 两个用途：① 决定能否从上游回填当天数据——周末与凌晨不行，那时上游返回的是上一个
// 交易日的数据；② 决定缓冲是否切换到新交易日——缓冲按交易日保留，周五收盘后到周一
// 开盘前都属于同一个交易日，不能按自然日零点清空。
func isTradingDayStarted(t time.Time) bool {
	switch t.Weekday() {
	case time.Saturday, time.Sunday:
		return false
	}
	return t.Hour()*60+t.Minute() >= stockUnusualTradingStartMinute
}

// backfillIfNeeded 为每个市场补一次当天全量，只处理尚未初始化的市场。
// 失败只记日志：非采集窗口内的失败不影响客户端降级判断。
func (handler *stockUnusualSSEHandler) backfillIfNeeded(now time.Time) {
	handler.mu.Lock()
	knownDate := handler.date
	handler.mu.Unlock()

	// 非交易时段（周末、次日凌晨）只有在「记得上次标注的是哪个交易日」时才回填：
	// 上游此时返回的就是那一批数据，沿用同一标注即可，不需要判断今天几号。
	// 连上次标注都没有（首次部署就赶上周末）则不猜，宁可空着也不标错日期。
	if !isTradingDayStarted(now) && knownDate == "" {
		return
	}
	for _, market := range stockUnusualMarkets {
		handler.mu.Lock()
		handler.resetDayLocked(now)
		done := handler.initialized[market]
		handler.mu.Unlock()
		if done {
			continue
		}
		if err := handler.backfillMarket(market); err != nil {
			log.Printf("stock unusual backfill outside trading window failed: market=%d: %v", market, err)
		}
	}
}

// idleWait 计算非交易时段的休眠时长：不超过一分钟，且不越过下一次交易窗口起点。
func (handler *stockUnusualSSEHandler) idleWait(now time.Time) time.Duration {
	wait := stockUnusualIdleWait
	nextOpen := time.Date(now.Year(), now.Month(), now.Day(), 9, 15, 0, 0, shanghaiLocation)
	if now.Before(nextOpen) {
		if until := nextOpen.Sub(now); until < wait {
			wait = until
		}
	}
	if wait <= 0 {
		return time.Second
	}
	return wait
}

// inStockUnusualTradingWindow 判断是否处于异动采集窗口：工作日 09:15-15:05，含首尾。
func inStockUnusualTradingWindow(t time.Time) bool {
	switch t.Weekday() {
	case time.Saturday, time.Sunday:
		return false
	}
	minute := t.Hour()*60 + t.Minute()
	return minute >= stockUnusualTradingStartMinute && minute <= stockUnusualTradingEndMinute
}

func (handler *stockUnusualSSEHandler) pollMarket(market uint8, now time.Time) {
	handler.mu.Lock()
	handler.resetDayLocked(now)
	initialized := handler.initialized[market]
	start := handler.cursors[market]
	handler.mu.Unlock()

	if !initialized {
		// 首次采集先回填当天全量：服务重启后缓冲仍能覆盖当天已发生的事件，客户端带游标重连才能补齐。
		if err := handler.backfillMarket(market); err != nil {
			handler.handlePollError(err)
			return
		}
		handler.markHealthy()
		return
	}

	for {
		items, err := handler.queryMarketMonitor(market, start, stockUnusualMonitorCount)
		if err != nil {
			handler.handlePollError(err)
			return
		}
		handler.ingest(market, start, items, true)
		if uint32(len(items)) < stockUnusualMonitorCount {
			break
		}
		start += uint32(len(items))
	}
	handler.markHealthy()
}

// backfillMarket 从偏移 0 顺序拉取当天全量异动写入缓冲。
//
// 回填期间照常广播：订阅可能早于回填完成（服务重启后 app 正好在连），若只填缓冲不推送，
// 这些客户端拿到的历史会缺掉「订阅时刻之后回填出来的部分」。客户端按 uniqueKey 去重，
// 因此先补发后广播造成的少量重复不会重复入库。
func (handler *stockUnusualSSEHandler) backfillMarket(market uint8) error {
	handler.mu.Lock()
	if handler.initialized[market] {
		handler.mu.Unlock()
		return nil
	}
	start := handler.cursors[market]
	handler.mu.Unlock()

	for {
		items, err := handler.queryMarketMonitor(market, start, stockUnusualMonitorCount)
		if err != nil {
			return err
		}
		if len(items) == 0 {
			break
		}
		handler.ingest(market, start, items, true)
		start += uint32(len(items))
		if uint32(len(items)) < stockUnusualMonitorCount {
			break
		}
	}

	handler.mu.Lock()
	handler.initialized[market] = true
	handler.mu.Unlock()
	return nil
}

// resetDayLocked 在跨日时清空当天缓冲与游标；seq 保持单调递增，客户端据此识别是否需要重置。
func (handler *stockUnusualSSEHandler) resetDayLocked(now time.Time) {
	if !isTradingDayStarted(now) {
		// 周末与开盘前的凌晨不切换：缓冲要保留到下一个交易日开盘，期间打开 app 仍能看到
		// 上一个交易日的全部异动。
		return
	}
	today := now.Format(time.DateOnly)
	if handler.date == today {
		return
	}
	handler.date = today
	handler.persistBufferDate(today)
	handler.events = nil
	handler.seen = make(map[unusualEventKey]struct{})
	handler.initialized = make(map[uint8]bool)
	handler.cursors = make(map[uint8]uint32)
}

// ingest 把一批新事件写入缓冲；broadcast 为 true 时同时广播给订阅者。
func (handler *stockUnusualSSEHandler) ingest(market uint8, start uint32, items []proto.MACMarketMonitorItem, broadcast bool) {
	handler.mu.Lock()
	defer handler.mu.Unlock()

	nextStart := start + uint32(len(items))
	for _, item := range items {
		if next := uint32(item.Index) + 1; next > nextStart {
			nextStart = next
		}
	}
	if current := handler.cursors[market]; nextStart > current {
		handler.cursors[market] = nextStart
	}

	for _, item := range items {
		key := makeUnusualEventKey(market, item)
		if _, seen := handler.seen[key]; seen {
			continue
		}
		handler.seen[key] = struct{}{}

		handler.seq++
		stored := unusualStoredEvent{Seq: handler.seq, Market: market, Date: handler.date, Item: item}
		handler.events = append(handler.events, stored)
		if excess := len(handler.events) - stockUnusualEventBufferLimit; excess > 0 {
			handler.events = handler.events[excess:]
		}
		if !broadcast {
			continue
		}

		data, err := json.Marshal(makeUnusualEventPayload(stored))
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
}

func (handler *stockUnusualSSEHandler) queryMarketMonitor(market uint8, start uint32, count uint32) ([]proto.MACMarketMonitorItem, error) {
	if handler.client == nil {
		return nil, fmt.Errorf("MAC 行情客户端不可用")
	}
	var lastErr error
	// 不在这里断开连接：Client 自带连接池、心跳、补建与熔断，Disconnect 会关闭整个客户端，
	// 连续两次查询后所有请求都会得到 client closed。
	for attempt := 1; attempt <= stockUnusualPollAttempts; attempt++ {
		items, err := handler.client.MACMarketMonitor(market, start, count)
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

func (handler *stockUnusualSSEHandler) markHealthy() {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	handler.failures = 0
	handler.lastError = ""
	handler.lastPollAt = handler.now()
}

func (handler *stockUnusualSSEHandler) handlePollError(pollErr error) {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	handler.failures++
	handler.lastError = pollErr.Error()
	if handler.failures > 1 {
		return
	}
	log.Printf("stock unusual poll failed: %v", pollErr)

	message := stockUnusualErrorMessage()
	for subscriber := range handler.subscribers {
		handler.enqueueLocked(subscriber, message)
	}
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

func makeUnusualEventPayload(event unusualStoredEvent) unusualEventPayload {
	item := event.Item
	payload := unusualEventPayload{
		Seq:   event.Seq,
		Date:  event.Date,
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
	if item.HasAmount {
		amount := finiteJSONFloat(item.Amount)
		payload.Amount = &amount
	}
	return payload
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
		// 订阅通道被写满说明客户端处理不过来，主动断开让它带游标重连，由补发逻辑补齐。
		delete(handler.subscribers, subscriber)
		close(subscriber.done)
	}
}

func stockUnusualErrorMessage() sseMessage {
	return sseMessage{event: "error", data: []byte(`{"message":"MAC 行情轮询失败"}`)}
}

func resetMessage(baseSeq uint64) sseMessage {
	return sseMessage{event: "reset", data: []byte(fmt.Sprintf(`{"baseSeq":%d}`, baseSeq))}
}

func readyMessage(seq uint64) sseMessage {
	return sseMessage{event: "ready", data: []byte(fmt.Sprintf(`{"seq":%d}`, seq))}
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
