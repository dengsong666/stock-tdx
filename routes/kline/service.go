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
	maxBarsPerRequest = 20_000
	maxSearchOffset   = uint32(10_000_000)
)

// ErrTooManyBars 表示查询区间超过单次最多 20,000 根 K 线的限制。
var ErrTooManyBars = errors.New("单次最多返回 20000 根K线，请缩小时间区间")

type macBarsClient interface {
	MACSymbolBars(market uint8, code string, period uint16, times uint16, start uint32, count uint32, adjust uint16) ([]proto.MACSymbolBar, error)
	Disconnect() error
}

type macClientFactory func(host string, timeoutSec int) macBarsClient

// Service 通过独立短连接查询单股票 K 线，并在多个 MAC 主站之间故障转移。
type Service struct {
	hosts      []string
	timeoutSec int
	newClient  macClientFactory
	now        func() time.Time

	mu        sync.Mutex
	preferred int
}

// NewService 创建使用内置 MAC 主站列表的生产 K 线服务。
func NewService() *Service {
	return newService(gotdx.MACHostAddresses(), defaultTimeoutSec, newMACClient, time.Now)
}

// newService 创建可注入主站、客户端工厂和时钟的 K 线服务。
func newService(hosts []string, timeoutSec int, factory macClientFactory, now func() time.Time) *Service {
	return &Service{
		hosts:      append([]string(nil), hosts...),
		timeoutSec: timeoutSec,
		newClient:  factory,
		now:        now,
	}
}

// newMACClient 创建只连接指定主站的 TDX MAC 短连接客户端。
func newMACClient(host string, timeoutSec int) macBarsClient {
	return gotdx.NewMAC(
		gotdx.WithMacTCPAddress(host),
		gotdx.WithMacTCPAddressPool(),
		gotdx.WithTimeoutSec(timeoutSec),
	)
}

// Fetch 查询时间区间内的 K 线；返回值按时间正序排列。
func (service *Service) Fetch(query Query) ([]Bar, error) {
	if len(service.hosts) == 0 || service.newClient == nil {
		return nil, fmt.Errorf("没有配置 MAC 行情主站")
	}

	preferred := service.preferredHost()
	var lastErr error
	for offset := range service.hosts {
		index := (preferred + offset) % len(service.hosts)
		host := service.hosts[index]
		client := service.newClient(host, service.timeoutSec)
		rawBars, err := queryRange(client, query)
		_ = client.Disconnect()
		if err == nil {
			service.setPreferredHost(index)
			return normalizeBars(rawBars, query.Period, service.currentTime()), nil
		}
		if errors.Is(err, ErrTooManyBars) {
			return nil, err
		}
		lastErr = fmt.Errorf("host=%s: %w", host, err)
		log.Printf("TDX K线主站失败 host=%s code=%s period=%s: %v", host, query.Code, query.Period.Name, err)
	}
	return nil, lastErr
}

// queryRange 将时间区间转换为 TDX 的历史偏移并分页读取。
func queryRange(client macBarsClient, query Query) ([]proto.MACSymbolBar, error) {
	startOffset, found, err := locateEndOffset(client, query)
	if err != nil || !found {
		return nil, err
	}

	result := make([]proto.MACSymbolBar, 0)
	seen := make(map[int64]struct{})
	offset := startOffset
	for {
		page, err := requestBars(client, query, offset, rangePageSize)
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
		if reachedStart || uint32(len(page)) < rangePageSize {
			break
		}
		if offset > math.MaxUint32-rangePageSize {
			break
		}
		offset += rangePageSize
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
func barAt(client macBarsClient, query Query, offset uint32) (*proto.MACSymbolBar, error) {
	items, err := requestBars(client, query, offset, 1)
	if err != nil || len(items) == 0 {
		return nil, err
	}
	item := items[0]
	return &item, nil
}

// requestBars 使用规范化查询参数调用 gotdx 已有公开 API。
func requestBars(client macBarsClient, query Query, offset uint32, count uint32) ([]proto.MACSymbolBar, error) {
	return client.MACSymbolBars(
		query.Market,
		query.Code,
		query.Period.Category,
		query.Period.Times,
		offset,
		count,
		query.Adjust,
	)
}

// normalizeBars 排序、规范涨跌字段并补充周期完成状态。
func normalizeBars(items []proto.MACSymbolBar, period Period, now time.Time) []Bar {
	sort.Slice(items, func(left int, right int) bool {
		return wallTimeInShanghai(items[left].DateTime).Before(wallTimeInShanghai(items[right].DateTime))
	})
	result := make([]Bar, 0, len(items))
	for index, item := range items {
		itemTime := wallTimeInShanghai(item.DateTime)
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
			Time:       itemTime.Format(dateTimeLayout),
			Open:       item.Open,
			High:       item.High,
			Low:        item.Low,
			Close:      item.Close,
			Volume:     item.Vol,
			Amount:     item.Amount,
			Turnover:   item.Turnover,
			Previous:   previous,
			Change:     change,
			ChangePct:  changePct,
			IsComplete: isBarComplete(period, itemTime, now),
		})
	}
	return result
}

// currentTime 返回可测试的当前北京时间。
func (service *Service) currentTime() time.Time {
	if service.now == nil {
		return time.Now().In(shanghaiLocation)
	}
	return service.now().In(shanghaiLocation)
}

// preferredHost 返回当前优先主站下标。
func (service *Service) preferredHost() int {
	service.mu.Lock()
	defer service.mu.Unlock()
	if len(service.hosts) == 0 {
		return 0
	}
	return service.preferred % len(service.hosts)
}

// setPreferredHost 记录最近一次成功的主站。
func (service *Service) setPreferredHost(index int) {
	service.mu.Lock()
	service.preferred = index
	service.mu.Unlock()
}
