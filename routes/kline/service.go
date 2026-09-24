package kline

import (
	"errors"
	"fmt"
	"log"
	"math"
	"sort"
	"sync"
	"time"

	gotdx "github.com/bensema/gotdx"
	"github.com/bensema/gotdx/proto"
)

const (
	defaultTimeoutSec = 3
	rangePageSize     = uint32(800)
	mainIndexPageSize = uint32(100)
	// 扩展行情单次最多返回 699 根，请求更大数量也会被截断。
	exIndexPageSize   = uint32(699)
	maxBarsPerRequest = 20_000
	maxSearchOffset   = uint32(10_000_000)
	maxLiveBarLag     = 2 * time.Minute
)

const (
	// exIndexCategory 是扩展行情协议里“中证指数”的分类号，中证系指数只在该协议提供。
	exIndexCategory = uint8(62)
	// 扩展行情把指数的成交额按百万元、成交量按手的不同倍数取整返回：分钟周期是万手，日线及以上是百万手。
	// 换算后与东财、传统主站一致，都是元与手，只是扩展行情的有效位数更少。
	exAmountScale       = 1_000_000
	exMinuteVolumeScale = 10_000
	exDailyVolumeScale  = 1_000_000
)

// indexProtocol 区分指数 K 线的两套取数协议。
type indexProtocol uint8

const (
	// indexProtocolMain 是传统主行情协议的 GetIndexBars，覆盖沪深北大部分指数。
	indexProtocolMain indexProtocol = iota
	// indexProtocolEx 是扩展行情协议的 ExGetKLine，中证系指数（如中证全指）只在此提供。
	indexProtocolEx
)

// ErrTooManyBars 表示查询区间超过单次最多 20,000 根 K 线的限制。
var ErrTooManyBars = errors.New("单次最多返回 20000 根K线，请缩小时间区间")

var errStaleLiveData = errors.New("实时分钟线已过期")

type macBarsClient interface {
	MACSymbolBars(market uint8, code string, period uint16, times uint16, start uint32, count uint32, adjust uint16) ([]proto.MACSymbolBar, error)
	GetIndexBars(category uint16, market uint8, code string, start uint16, count uint16) (*proto.GetIndexBarsReply, error)
	ExGetKLine(category uint8, code string, period uint16, start uint32, count uint16, times uint16) (*proto.ExGetKLineReply, error)
	Disconnect() error
}

type barsClientFactory func(host string, timeoutSec int) macBarsClient

// barsRoute 描述一条取数链路：主站列表、短连接客户端工厂与所用协议。
type barsRoute struct {
	hosts    []string
	factory  barsClientFactory
	protocol indexProtocol
}

// Service 通过独立短连接查询单股票 K 线，并在多个 MAC 主站之间故障转移。
type Service struct {
	stockHosts       []string
	indexHosts       []string
	exIndexHosts     []string
	timeoutSec       int
	newStockClient   barsClientFactory
	newIndexClient   barsClientFactory
	newExIndexClient barsClientFactory
	now              func() time.Time

	mu             sync.Mutex
	preferredStock int
	preferredIndex int
	preferredEx    int
}

// NewService 创建使用内置 MAC 主站列表的生产 K 线服务。
func NewService() *Service {
	return &Service{
		stockHosts:       append([]string(nil), gotdx.MACHostAddresses()...),
		indexHosts:       append([]string(nil), gotdx.MainHostAddresses()...),
		exIndexHosts:     append([]string(nil), gotdx.ExHostAddresses()...),
		timeoutSec:       defaultTimeoutSec,
		newStockClient:   newMACClient,
		newIndexClient:   newMainClient,
		newExIndexClient: newExIndexClient,
		now:              time.Now,
	}
}

// newService 创建可注入主站和客户端工厂的 K 线服务。
func newService(hosts []string, timeoutSec int, factory barsClientFactory) *Service {
	return &Service{
		stockHosts:       append([]string(nil), hosts...),
		indexHosts:       append([]string(nil), hosts...),
		exIndexHosts:     append([]string(nil), hosts...),
		timeoutSec:       timeoutSec,
		newStockClient:   factory,
		newIndexClient:   factory,
		newExIndexClient: factory,
		now:              time.Now,
	}
}

// rawBar 是股票与指数协议响应转换后的内部统一结构。
type rawBar struct {
	DateTime time.Time
	Open     float64
	High     float64
	Low      float64
	Close    float64
	Volume   float64
	Amount   float64
	Turnover *float64
	PreClose float64
}

// newMACClient 创建只连接指定主站的 TDX MAC 短连接客户端。
func newMACClient(host string, timeoutSec int) macBarsClient {
	return gotdx.NewMAC(
		gotdx.WithMacTCPAddress(host),
		gotdx.WithMacTCPAddressPool(),
		gotdx.WithTimeoutSec(timeoutSec),
	)
}

// newMainClient 创建只连接指定传统主行情服务器的短连接客户端，供指数协议使用。
func newMainClient(host string, timeoutSec int) macBarsClient {
	return gotdx.New(
		gotdx.WithTCPAddress(host),
		gotdx.WithTCPAddressPool(),
		gotdx.WithTimeoutSec(timeoutSec),
	)
}

// exIndexClient 在扩展行情客户端之上补一次登录握手：扩展市场协议不会懒连接，未握手时取数直接报 connection is nil。
type exIndexClient struct {
	*gotdx.Client
	connected bool
}

// newExIndexClient 创建只连接指定扩展行情服务器的短连接客户端，供中证系指数取数。
func newExIndexClient(host string, timeoutSec int) macBarsClient {
	return &exIndexClient{Client: gotdx.NewEx(
		gotdx.WithExTCPAddress(host),
		gotdx.WithExTCPAddressPool(),
		gotdx.WithTimeoutSec(timeoutSec),
	)}
}

// ExGetKLine 首次取数前完成扩展市场登录，之后复用同一条短连接。
func (wrapper *exIndexClient) ExGetKLine(category uint8, code string, period uint16, start uint32, count uint16, times uint16) (*proto.ExGetKLineReply, error) {
	if !wrapper.connected {
		if _, err := wrapper.Client.ConnectEx(); err != nil {
			return nil, err
		}
		wrapper.connected = true
	}
	return wrapper.Client.ExGetKLine(category, code, period, start, count, times)
}

// Fetch 查询时间区间内的 K 线；返回值按时间正序排列。
func (service *Service) Fetch(query Query) ([]Bar, error) {
	bars, err := service.fetch(query, service.route(query.Type, indexProtocolMain))
	if len(bars) > 0 || query.Type != assetIndex {
		return bars, err
	}
	// 传统主站没有这个指数时改走扩展行情：中证系指数（如中证全指）只存在于扩展行情协议。
	exBars, exErr := service.fetch(query, service.route(assetIndex, indexProtocolEx))
	if len(exBars) > 0 {
		return exBars, nil
	}
	if exErr != nil {
		return nil, exErr
	}
	return bars, err
}

// fetch 在一条取数链路上完成故障转移查询；返回空切片表示该链路没有该标的的数据。
func (service *Service) fetch(query Query, route barsRoute) ([]Bar, error) {
	if len(route.hosts) == 0 || route.factory == nil {
		return nil, fmt.Errorf("没有配置可用行情主站")
	}

	preferred := service.preferredHost(query.Type, route.protocol, len(route.hosts))
	var lastErr error
	for offset := range route.hosts {
		index := (preferred + offset) % len(route.hosts)
		host := route.hosts[index]
		client := route.factory(host, service.timeoutSec)
		requestQuery := query
		if query.Type == assetIndex && query.Period.Name == "3m" && route.protocol == indexProtocolMain {
			requestQuery.Start = requestQuery.Start.Add(-3 * time.Minute)
		}
		rawBars, err := queryRange(client, requestQuery, route.protocol)
		_ = client.Disconnect()
		if err == nil {
			if freshnessErr := service.validateLiveFreshness(query, rawBars); freshnessErr != nil {
				lastErr = fmt.Errorf("host=%s: %w", host, freshnessErr)
				log.Printf("TDX K线主站数据过期 host=%s code=%s period=%s: %v", host, query.Code, query.Period.Name, freshnessErr)
				continue
			}
			if query.Type == assetIndex && query.Period.Name == "3m" && route.protocol == indexProtocolMain {
				rawBars = aggregateIndexThreeMinuteBars(rawBars)
			}
			service.setPreferredHost(query.Type, route.protocol, index)
			return normalizeBars(rawBars, query.Start, query.End), nil
		}
		if errors.Is(err, ErrTooManyBars) {
			return nil, err
		}
		lastErr = fmt.Errorf("host=%s: %w", host, err)
		log.Printf("TDX K线主站失败 host=%s code=%s period=%s: %v", host, query.Code, query.Period.Name, err)
	}
	return nil, lastErr
}

// validateLiveFreshness 拒绝当天交易时段内明显落后的分钟线，促使调用方切换主站。
func (service *Service) validateLiveFreshness(query Query, bars []rawBar) error {
	if query.Period.Kind != periodMinute {
		return nil
	}
	now := service.now().In(shanghaiLocation)
	end := query.End.In(shanghaiLocation)
	if !sameDate(now, end) || !isTradingClock(end) || end.Before(now.Add(-10*time.Minute)) || end.After(now.Add(2*time.Minute)) {
		return nil
	}
	if len(bars) == 0 {
		return errStaleLiveData
	}
	latest := bars[0].DateTime
	for _, bar := range bars[1:] {
		if bar.DateTime.After(latest) {
			latest = bar.DateTime
		}
	}
	lag := end.Sub(wallTimeInShanghai(latest))
	if lag > maxLiveBarLag {
		return fmt.Errorf("最新K线=%s，查询结束=%s，落后=%s", wallTimeInShanghai(latest).Format(dateTimeLayout), end.Format(dateTimeLayout), lag.Round(time.Second))
	}
	return nil
}

func isTradingClock(value time.Time) bool {
	totalSeconds := value.Hour()*3600 + value.Minute()*60 + value.Second()
	return totalSeconds >= 9*3600+30*60 && totalSeconds <= 11*3600+31*60 ||
		totalSeconds >= 13*3600 && totalSeconds <= 15*3600+1*60
}

// route 按资产类型和指数协议选择主站列表与客户端工厂。
func (service *Service) route(asset assetType, protocol indexProtocol) barsRoute {
	if asset == assetIndex {
		if protocol == indexProtocolEx {
			return barsRoute{hosts: service.exIndexHosts, factory: service.newExIndexClient, protocol: indexProtocolEx}
		}
		return barsRoute{hosts: service.indexHosts, factory: service.newIndexClient, protocol: indexProtocolMain}
	}
	return barsRoute{hosts: service.stockHosts, factory: service.newStockClient, protocol: indexProtocolMain}
}

// pageSizeFor 返回该链路单次读取的 K 线根数：传统主行情协议的指数大批量请求会被部分主站拒绝。
func pageSizeFor(query Query, protocol indexProtocol) uint32 {
	switch {
	case protocol == indexProtocolEx:
		return exIndexPageSize
	case query.Type == assetIndex:
		return mainIndexPageSize
	default:
		return rangePageSize
	}
}

// queryRange 将时间区间转换为 TDX 的历史偏移并分页读取。
func queryRange(client macBarsClient, query Query, protocol indexProtocol) ([]rawBar, error) {
	fetchPage := func(offset uint32, count uint32) ([]rawBar, error) {
		return requestBars(client, query, protocol, offset, count)
	}
	startOffset, found, err := locateEndOffset(fetchPage, query)
	if err != nil || !found {
		return nil, err
	}

	result := make([]rawBar, 0)
	seen := make(map[int64]struct{})
	offset := startOffset
	pageSize := pageSizeFor(query, protocol)
	for {
		page, err := fetchPage(offset, pageSize)
		if err != nil {
			return nil, err
		}
		if len(page) == 0 {
			break
		}

		reachedStart := false
		for _, item := range page {
			itemTime := wallTimeInShanghai(item.DateTime)
			if itemTime.Before(query.Start) {
				reachedStart = true
				continue
			}
			if itemTime.After(query.End) {
				continue
			}
			key := itemTime.Unix()
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			result = append(result, item)
			if len(result) > maxBarsPerRequest {
				return nil, ErrTooManyBars
			}
		}
		if reachedStart || uint32(len(page)) < pageSize {
			break
		}
		if offset > math.MaxUint32-pageSize {
			break
		}
		offset += pageSize
	}
	return result, nil
}

// barsPage 按历史偏移读取一页 K 线：两套指数协议都以最新一根为偏移 0，向历史递增。
type barsPage func(offset uint32, count uint32) ([]rawBar, error)

// locateEndOffset 使用指数搜索和二分搜索定位不晚于 end 的第一根 K 线偏移。
func locateEndOffset(fetchPage barsPage, query Query) (uint32, bool, error) {
	latest, err := firstBar(fetchPage, 0)
	if err != nil || latest == nil {
		return 0, false, err
	}
	if !wallTimeInShanghai(latest.DateTime).After(query.End) {
		return 0, true, nil
	}

	low := uint32(0)
	high := uint32(1)
	for {
		item, err := firstBar(fetchPage, high)
		if err != nil {
			return 0, false, err
		}
		if item == nil || !wallTimeInShanghai(item.DateTime).After(query.End) {
			break
		}
		low = high
		if high >= maxSearchOffset/2 {
			high = maxSearchOffset
			break
		}
		high *= 2
	}

	for low+1 < high {
		middle := low + (high-low)/2
		item, err := firstBar(fetchPage, middle)
		if err != nil {
			return 0, false, err
		}
		if item == nil || !wallTimeInShanghai(item.DateTime).After(query.End) {
			high = middle
		} else {
			low = middle
		}
	}
	candidate, err := firstBar(fetchPage, high)
	if err != nil || candidate == nil {
		return 0, false, err
	}
	if wallTimeInShanghai(candidate.DateTime).After(query.End) {
		return 0, false, nil
	}
	return high, true, nil
}

// firstBar 查询指定历史偏移上的一根 K 线。
func firstBar(fetchPage barsPage, offset uint32) (*rawBar, error) {
	items, err := fetchPage(offset, 1)
	if err != nil || len(items) == 0 {
		return nil, err
	}
	item := items[0]
	return &item, nil
}

// requestBars 按链路协议读取一页 K 线，并转换成统一的内部结构。
func requestBars(client macBarsClient, query Query, protocol indexProtocol, offset uint32, count uint32) ([]rawBar, error) {
	if query.Type == assetIndex {
		if protocol == indexProtocolEx {
			return requestExIndexBars(client, query, offset, count)
		}
		if offset > math.MaxUint16 || count > math.MaxUint16 {
			return nil, fmt.Errorf("指数 K 线历史偏移超出协议范围")
		}
		reply, err := client.GetIndexBars(
			query.Period.IndexCategory,
			query.Market,
			query.Code,
			uint16(offset),
			uint16(count),
		)
		if err != nil || reply == nil {
			return nil, err
		}
		result := make([]rawBar, 0, len(reply.List))
		for _, item := range reply.List {
			result = append(result, rawBar{
				DateTime: item.DateTime,
				Open:     item.Open, High: item.High, Low: item.Low, Close: item.Close,
				Volume: item.Vol, Amount: item.Amount, PreClose: item.PreClose,
			})
		}
		return result, nil
	}

	items, err := client.MACSymbolBars(
		query.Market, query.Code, query.Period.Category, query.Period.Times,
		offset, count, query.Adjust,
	)
	if err != nil {
		return nil, err
	}
	result := make([]rawBar, 0, len(items))
	for _, item := range items {
		turnover := item.Turnover
		result = append(result, rawBar{
			DateTime: item.DateTime,
			Open:     item.Open, High: item.High, Low: item.Low, Close: item.Close,
			Volume: item.Vol, Amount: item.Amount, Turnover: &turnover, PreClose: item.PreClose,
		})
	}
	return result, nil
}

// requestExIndexBars 用扩展行情协议读取中证系指数，并把量额换算成东财、传统主站口径的元与手。
// 该协议自带倍率参数，指数 3 分钟无需本地聚合。
func requestExIndexBars(client macBarsClient, query Query, offset uint32, count uint32) ([]rawBar, error) {
	if count > math.MaxUint16 {
		return nil, fmt.Errorf("指数 K 线单页数量超出协议范围")
	}
	reply, err := client.ExGetKLine(
		exIndexCategory,
		query.Code,
		query.Period.IndexCategory,
		offset,
		uint16(count),
		query.Period.Times,
	)
	if err != nil || reply == nil {
		return nil, err
	}
	volumeScale := uint32(exDailyVolumeScale)
	if query.Period.Kind == periodMinute {
		volumeScale = exMinuteVolumeScale
	}
	result := make([]rawBar, 0, len(reply.List))
	for _, item := range reply.List {
		result = append(result, rawBar{
			DateTime: item.DateTime,
			Open:     item.Open, High: item.High, Low: item.Low, Close: item.Close,
			Volume: float64(item.Vol) * float64(volumeScale), Amount: item.Amount * exAmountScale,
			PreClose: item.PreClose,
		})
	}
	return result, nil
}

// normalizeBars 排序、筛选区间并规范涨跌字段。
func normalizeBars(items []rawBar, start time.Time, end time.Time) []Bar {
	sort.Slice(items, func(left int, right int) bool {
		return wallTimeInShanghai(items[left].DateTime).Before(wallTimeInShanghai(items[right].DateTime))
	})
	result := make([]Bar, 0, len(items))
	for index, item := range items {
		itemTime := wallTimeInShanghai(item.DateTime)
		if itemTime.Before(start) || itemTime.After(end) {
			continue
		}
		previous := item.PreClose
		if index > 0 {
			previous = items[index-1].Close
		}
		change := item.Close - previous
		changePct := 0.0
		if previous != 0 {
			changePct = change / previous * 100
		}
		result = append(result, Bar{
			Time:      itemTime.Format(dateTimeLayout),
			Open:      item.Open,
			High:      item.High,
			Low:       item.Low,
			Close:     item.Close,
			Volume:    item.Volume,
			Amount:    item.Amount,
			Turnover:  item.Turnover,
			Previous:  previous,
			Change:    change,
			ChangePct: changePct,
		})
	}
	return result
}

// aggregateIndexThreeMinuteBars 将指数 1 分钟数据按沪深北交易时段聚合成 3 分钟。
func aggregateIndexThreeMinuteBars(items []rawBar) []rawBar {
	sort.Slice(items, func(left int, right int) bool {
		return wallTimeInShanghai(items[left].DateTime).Before(wallTimeInShanghai(items[right].DateTime))
	})
	result := make([]rawBar, 0, (len(items)+2)/3)
	var current *rawBar
	currentKey := ""
	for _, item := range items {
		item.DateTime = wallTimeInShanghai(item.DateTime)
		key := threeMinuteBucketKey(item.DateTime)
		if current == nil || key != currentKey {
			if current != nil {
				result = append(result, *current)
			}
			copyItem := item
			current = &copyItem
			currentKey = key
			continue
		}
		if item.High > current.High {
			current.High = item.High
		}
		if item.Low < current.Low {
			current.Low = item.Low
		}
		current.Close = item.Close
		current.DateTime = item.DateTime
		current.Volume += item.Volume
		current.Amount += item.Amount
		if current.Turnover != nil && item.Turnover != nil {
			turnover := *current.Turnover + *item.Turnover
			current.Turnover = &turnover
		} else {
			current.Turnover = nil
		}
	}
	if current != nil {
		result = append(result, *current)
	}
	return result
}

// threeMinuteBucketKey 返回不跨午间休市的 3 分钟分组键。
func threeMinuteBucketKey(value time.Time) string {
	value = wallTimeInShanghai(value)
	hour, minute := value.Hour(), value.Minute()
	session := "morning"
	anchorMinutes := 9*60 + 31
	if hour >= 13 {
		session = "afternoon"
		anchorMinutes = 13*60 + 1
	}
	offset := hour*60 + minute - anchorMinutes
	if offset < 0 {
		offset = 0
	}
	return fmt.Sprintf("%04d%02d%02d-%s-%d", value.Year(), value.Month(), value.Day(), session, offset/3)
}

// preferredHost 返回该链路当前优先主站下标。
func (service *Service) preferredHost(asset assetType, protocol indexProtocol, hostCount int) int {
	service.mu.Lock()
	defer service.mu.Unlock()
	if hostCount == 0 {
		return 0
	}
	return *service.preferenceSlot(asset, protocol) % hostCount
}

// setPreferredHost 记录该链路最近一次成功的主站下标。
func (service *Service) setPreferredHost(asset assetType, protocol indexProtocol, index int) {
	service.mu.Lock()
	*service.preferenceSlot(asset, protocol) = index
	service.mu.Unlock()
}

// preferenceSlot 返回该链路对应的优先主站记录，调用方必须持有 service.mu。
func (service *Service) preferenceSlot(asset assetType, protocol indexProtocol) *int {
	if asset != assetIndex {
		return &service.preferredStock
	}
	if protocol == indexProtocolEx {
		return &service.preferredEx
	}
	return &service.preferredIndex
}
