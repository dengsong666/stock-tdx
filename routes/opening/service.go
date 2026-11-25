package opening

import (
	"fmt"
	"log"
	"math"
	"sort"
	"strings"
	"sync"

	gotdx "github.com/bensema/gotdx"
	"github.com/bensema/gotdx/proto"
)

const (
	defaultTimeoutSec = 10
	// transactionsLimit 是单只股票单日分笔的拉取上限：3 秒快照一天约 4800 条，取整天才能翻到开盘那头
	// （MAC 分笔从收盘往前分页，start=0 是收盘前最后一页）。
	transactionsLimit = uint32(50000)
	auctionMinute     = "09:25"
	continuousStart   = "09:30:00"
)

// OpeningTrade 是一只股票某个交易日的集合竞价成交与连续竞价第一笔成交，价格单位为元（按分四舍五入）。
type OpeningTrade struct {
	// AuctionTime / AuctionPrice 是 09:25 集合竞价那条成交；当天没有竞价成交时两者为 null。
	AuctionTime  *string  `json:"auction_time"`
	AuctionPrice *float64 `json:"auction_price"`
	// FirstTime / FirstPrice / FirstVol 是 09:30:00 及以后的第一条分笔快照（约 3 秒一条，取快照内最后成交价），量单位为手。
	FirstTime  string  `json:"first_time"`
	FirstPrice float64 `json:"first_price"`
	FirstVol   uint32  `json:"first_vol"`
}

// FetchResult 同时返回查到的开盘成交和当天没有连续竞价成交的股票代码。
type FetchResult struct {
	Trades       map[string]OpeningTrade
	MissingCodes []string
}

type macTransactionsClient interface {
	MACTransactionsWithDate(market uint8, code string, start uint32, count uint32, queryDate uint32) ([]proto.MACTransactionItem, error)
	Disconnect() error
}

type macClientFactory func(host string, timeoutSec int) macTransactionsClient

// Service 通过 MAC 历史分笔查询开盘成交，在多个 MAC 主站之间故障转移。
type Service struct {
	hosts      []string
	timeoutSec int
	newClient  macClientFactory

	mu        sync.Mutex
	preferred int
}

// NewService 创建使用内置 MAC 主站列表的生产服务。
func NewService() *Service {
	return newService(gotdx.MACHostAddresses(), defaultTimeoutSec, newMACClient)
}

// newService 创建可注入主站和客户端工厂的服务，便于离线测试。
func newService(hosts []string, timeoutSec int, factory macClientFactory) *Service {
	return &Service{hosts: append([]string(nil), hosts...), timeoutSec: timeoutSec, newClient: factory}
}

// newMACClient 创建只连接指定主站的 MAC 短连接客户端。
func newMACClient(host string, timeoutSec int) macTransactionsClient {
	return gotdx.NewMAC(
		gotdx.WithMacTCPAddress(host),
		gotdx.WithMacTCPAddressPool(),
		gotdx.WithTimeoutSec(timeoutSec),
	)
}

// Fetch 顺序查询一批股票在 date（YYYYMMDD）当天的开盘成交。
//
// 一次请求复用一条短连接；某只股票查询失败时断开、换下一个主站新建连接重试，全部主站都失败才整批报错。
// 不能复用已断开的客户端：gotdx 的 Disconnect 会把客户端永久标记为关闭，之后每次调用都返回 "client closed"。
func (service *Service) Fetch(date uint32, codes []string) (FetchResult, error) {
	if len(service.hosts) == 0 || service.newClient == nil {
		return FetchResult{}, fmt.Errorf("没有配置 MAC 行情主站")
	}
	session := &session{service: service, index: service.preferredHost()}
	defer session.close()

	result := FetchResult{Trades: make(map[string]OpeningTrade, len(codes)), MissingCodes: []string{}}
	for _, code := range codes {
		market, err := marketForCode(code)
		if err != nil {
			return FetchResult{}, err
		}
		items, err := session.transactions(market, code, date)
		if err != nil {
			return FetchResult{}, fmt.Errorf("code=%s: %w", code, err)
		}
		trade, ok := openingTrade(items)
		if !ok {
			result.MissingCodes = append(result.MissingCodes, code)
			continue
		}
		result.Trades[code] = trade
	}
	return result, nil
}

// session 持有当前主站上的一条短连接，出错时换主站重建。
type session struct {
	service *Service
	index   int
	client  macTransactionsClient
}

// transactions 拉取整天分笔；从当前主站开始，每个主站最多试一次。
func (current *session) transactions(market uint8, code string, date uint32) ([]proto.MACTransactionItem, error) {
	hosts := current.service.hosts
	var lastErr error
	for attempt := 0; attempt < len(hosts); attempt++ {
		if current.client == nil {
			current.client = current.service.newClient(hosts[current.index], current.service.timeoutSec)
		}
		items, err := current.client.MACTransactionsWithDate(market, code, 0, transactionsLimit, date)
		if err == nil {
			current.service.setPreferredHost(current.index)
			return items, nil
		}
		lastErr = fmt.Errorf("host=%s: %w", hosts[current.index], err)
		log.Printf("TDX 开盘成交主站失败 host=%s code=%s: %v", hosts[current.index], code, err)
		current.close()
		current.index = (current.index + 1) % len(hosts)
	}
	return nil, lastErr
}

// close 断开当前连接；断开后的客户端不再复用。
func (current *session) close() {
	if current.client != nil {
		_ = current.client.Disconnect()
		current.client = nil
	}
}

// openingTrade 从整天分笔里取 09:25 竞价成交与 09:30:00 后第一条；没有连续竞价成交时返回 false。
//
// MAC 分笔按页从收盘往前取、页内时间升序，拼起来整体不是时间序，所以先按时间稳定排序；价格为 0 的占位记录跳过。
func openingTrade(items []proto.MACTransactionItem) (OpeningTrade, bool) {
	records := make([]proto.MACTransactionItem, 0, len(items))
	for _, item := range items {
		if item.Price > 0 {
			records = append(records, item)
		}
	}
	sort.SliceStable(records, func(left, right int) bool { return records[left].Time < records[right].Time })

	var trade OpeningTrade
	found := false
	for _, item := range records {
		if trade.AuctionPrice == nil && strings.HasPrefix(item.Time, auctionMinute) {
			auctionTime, auctionPrice := item.Time, roundCents(item.Price)
			trade.AuctionTime, trade.AuctionPrice = &auctionTime, &auctionPrice
		}
		if item.Time >= continuousStart {
			trade.FirstTime, trade.FirstPrice, trade.FirstVol = item.Time, roundCents(item.Price), item.Vol
			found = true
			break
		}
	}
	return trade, found
}

// roundCents 把单精度还原出的价格按分四舍五入。
func roundCents(price float64) float64 {
	return math.Round(price*100) / 100
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

// marketForCode 校验沪深北 A 股六位代码并推断 TDX 市场号（与 routes/auction 同一套前缀）。
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

// isASCIIDigits 判断字符串是否全是 ASCII 数字且非空。
func isASCIIDigits(value string) bool {
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return value != ""
}
