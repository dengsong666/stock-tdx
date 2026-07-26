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
	indexPageSize     = uint32(100)
	maxBarsPerRequest = 20_000
	maxSearchOffset   = uint32(10_000_000)
)

// ErrTooManyBars 表示查询区间超过单次最多 20,000 根 K 线的限制。
var ErrTooManyBars = errors.New("单次最多返回 20000 根K线，请缩小时间区间")

type macBarsClient interface {
	MACSymbolBars(market uint8, code string, period uint16, times uint16, start uint32, count uint32, adjust uint16) ([]proto.MACSymbolBar, error)
	GetIndexBars(category uint16, market uint8, code string, start uint16, count uint16) (*proto.GetIndexBarsReply, error)
	Disconnect() error
}

type barsClientFactory func(host string, timeoutSec int) macBarsClient

// Service 通过独立短连接查询单股票 K 线，并在多个 MAC 主站之间故障转移。
type Service struct {
	stockHosts     []string
	indexHosts     []string
	timeoutSec     int
	newStockClient barsClientFactory
	newIndexClient barsClientFactory

	mu             sync.Mutex
	preferredStock int
	preferredIndex int
}

// NewService 创建使用内置 MAC 主站列表的生产 K 线服务。
func NewService() *Service {
	return &Service{
		stockHosts:     append([]string(nil), gotdx.MACHostAddresses()...),
		indexHosts:     append([]string(nil), gotdx.MainHostAddresses()...),
		timeoutSec:     defaultTimeoutSec,
		newStockClient: newMACClient,
		newIndexClient: newMainClient,
	}
}

// newService 创建可注入主站和客户端工厂的 K 线服务。
func newService(hosts []string, timeoutSec int, factory barsClientFactory) *Service {
	return &Service{
		stockHosts:     append([]string(nil), hosts...),
		indexHosts:     append([]string(nil), hosts...),
		timeoutSec:     timeoutSec,
		newStockClient: factory,
		newIndexClient: factory,
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

// Fetch 查询时间区间内的 K 线；返回值按时间正序排列。
func (service *Service) Fetch(query Query) ([]Bar, error) {
	hosts, factory := service.connectionConfig(query.Type)
	if len(hosts) == 0 || factory == nil {
		return nil, fmt.Errorf("没有配置可用行情主站")
	}

	preferred := service.preferredHost(query.Type, len(hosts))
	var lastErr error
	for offset := range hosts {
		index := (preferred + offset) % len(hosts)
		host := hosts[index]
		client := factory(host, service.timeoutSec)
		requestQuery := query
		if query.Type == assetIndex && query.Period.Name == "3m" {
			requestQuery.Start = requestQuery.Start.Add(-3 * time.Minute)
		}
		rawBars, err := queryRange(client, requestQuery)
		_ = client.Disconnect()
		if err == nil {
			if query.Type == assetIndex && query.Period.Name == "3m" {
				rawBars = aggregateIndexThreeMinuteBars(rawBars)
			}
			service.setPreferredHost(query.Type, index)
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

// connectionConfig 为股票选择 MAC 主站，为指数选择传统主行情服务器。
func (service *Service) connectionConfig(asset assetType) ([]string, barsClientFactory) {
	if asset == assetIndex {
		return service.indexHosts, service.newIndexClient
	}
	return service.stockHosts, service.newStockClient
}

// queryRange 将时间区间转换为 TDX 的历史偏移并分页读取。
func queryRange(client macBarsClient, query Query) ([]rawBar, error) {
	startOffset, found, err := locateEndOffset(client, query)
	if err != nil || !found {
		return nil, err
	}

	result := make([]rawBar, 0)
	seen := make(map[int64]struct{})
	offset := startOffset
	pageSize := rangePageSize
	if query.Type == assetIndex {
		// GetIndexBars 的传统主行情协议大批量请求会被部分主站直接拒绝。
		pageSize = indexPageSize
	}
	for {
		page, err := requestBars(client, query, offset, pageSize)
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

// locateEndOffset 使用指数搜索和二分搜索定位不晚于 end 的第一根 K 线偏移。
func locateEndOffset(client macBarsClient, query Query) (uint32, bool, error) {
	latest, err := barAt(client, query, 0)
	if err != nil || latest == nil {
		return 0, false, err
	}
	if !wallTimeInShanghai(latest.DateTime).After(query.End) {
		return 0, true, nil
	}

	low := uint32(0)
	high := uint32(1)
	for {
		item, err := barAt(client, query, high)
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
		item, err := barAt(client, query, middle)
		if err != nil {
			return 0, false, err
		}
		if item == nil || !wallTimeInShanghai(item.DateTime).After(query.End) {
			high = middle
		} else {
			low = middle
		}
	}
	candidate, err := barAt(client, query, high)
	if err != nil || candidate == nil {
		return 0, false, err
	}
	if wallTimeInShanghai(candidate.DateTime).After(query.End) {
		return 0, false, nil
	}
	return high, true, nil
}

// barAt 查询指定历史偏移上的一根 K 线。
func barAt(client macBarsClient, query Query, offset uint32) (*rawBar, error) {
	items, err := requestBars(client, query, offset, 1)
	if err != nil || len(items) == 0 {
		return nil, err
	}
	item := items[0]
	return &item, nil
}

// requestBars 使用规范化查询参数调用 gotdx 已有公开 API。
func requestBars(client macBarsClient, query Query, offset uint32, count uint32) ([]rawBar, error) {
	if query.Type == assetIndex {
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

// preferredHost 返回当前优先主站下标。
func (service *Service) preferredHost(asset assetType, hostCount int) int {
	service.mu.Lock()
	defer service.mu.Unlock()
	if hostCount == 0 {
		return 0
	}
	if asset == assetIndex {
		return service.preferredIndex % hostCount
	}
	return service.preferredStock % hostCount
}

// setPreferredHost 记录最近一次成功的主站。
func (service *Service) setPreferredHost(asset assetType, index int) {
	service.mu.Lock()
	if asset == assetIndex {
		service.preferredIndex = index
	} else {
		service.preferredStock = index
	}
	service.mu.Unlock()
}
