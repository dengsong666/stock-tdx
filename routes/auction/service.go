package auction

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	gotdx "github.com/bensema/gotdx"
	"github.com/bensema/gotdx/proto"
	"github.com/bensema/gotdx/types"
)

const (
	defaultTimeoutSec = 3
	klinePageSize     = uint16(20)
	maxKLineOffset    = uint16(4000)
)

// AuctionAmount 是单个交易日的竞价金额，单位为万元。
type AuctionAmount struct {
	Date      string `json:"date"`
	AmountWan int64  `json:"amount_wan"`
}

// FetchResult 同时返回成功结果和完全没有有效竞价数据的股票代码。
type FetchResult struct {
	Auctions     map[string][]AuctionAmount
	MissingCodes []string
}

type auctionClient interface {
	Connect() (*proto.Hello1Reply, error)
	GetKLine(category uint16, market uint8, code string, start uint16, count uint16, times uint16, adjust uint16) (*proto.GetSecurityBarsReply, error)
	StockHistoryFullTransaction(date uint32, market uint8, code string) ([]proto.HistoryTransactionData, error)
	Disconnect() error
}

type clientFactory func() auctionClient

// Service 使用传统主行情协议查询一分钟 K 线和历史逐笔成交。
type Service struct {
	newClient clientFactory
}

// NewService 创建生产环境竞价金额服务。
func NewService() *Service {
	return newService(func() auctionClient {
		return gotdx.New(gotdx.WithTimeoutSec(defaultTimeoutSec))
	})
}

func newService(factory clientFactory) *Service {
	return &Service{newClient: factory}
}

// Fetch 顺序查询一批股票最近若干个有集合竞价成交的交易日。
func (service *Service) Fetch(codes []string, days int) (FetchResult, error) {
	if service == nil || service.newClient == nil {
		return FetchResult{}, fmt.Errorf("没有配置行情客户端")
	}
	client := service.newClient()
	if client == nil {
		return FetchResult{}, fmt.Errorf("行情客户端不可用")
	}
	defer client.Disconnect()
	if _, err := client.Connect(); err != nil {
		return FetchResult{}, err
	}

	result := FetchResult{Auctions: make(map[string][]AuctionAmount, len(codes))}
	for _, code := range codes {
		market, err := marketForCode(code)
		if err != nil {
			return FetchResult{}, err
		}
		amounts, err := fetchCode(client, market, code, days)
		if err != nil {
			return FetchResult{}, fmt.Errorf("code=%s: %w", code, err)
		}
		result.Auctions[code] = amounts
		if len(amounts) == 0 {
			result.MissingCodes = append(result.MissingCodes, code)
		}
	}
	return result, nil
}

func fetchCode(client auctionClient, market uint8, code string, days int) ([]AuctionAmount, error) {
	dates, err := findAuctionDates(client, market, code, days)
	if err != nil {
		return nil, err
	}
	amounts := make([]AuctionAmount, 0, len(dates))
	for _, date := range dates {
		items, err := client.StockHistoryFullTransaction(date, market, code)
		if err != nil {
			return nil, err
		}
		item, ok := auctionTransaction(items)
		if !ok {
			continue
		}
		amounts = append(amounts, AuctionAmount{
			Date:      formatDate(date),
			AmountWan: int64(math.Round(item.Price * float64(item.Vol) * 100 / 10000)),
		})
	}
	return amounts, nil
}

// findAuctionDates 通过原始 09:31 一分钟 K 线定位最近的有效交易日期。
func findAuctionDates(client auctionClient, market uint8, code string, days int) ([]uint32, error) {
	found := make(map[uint32]struct{}, days)
	for start := uint16(0); start < maxKLineOffset && len(found) < days; start += klinePageSize {
		reply, err := client.GetKLine(types.KLINE_TYPE_1MIN, market, code, start, klinePageSize, 1, types.AdjustNone)
		if err != nil {
			return nil, err
		}
		if reply == nil || len(reply.List) == 0 {
			break
		}
		for _, bar := range reply.List {
			if bar.DateTime.Hour() != 9 || bar.DateTime.Minute() != 31 {
				continue
			}
			value, err := strconv.ParseUint(bar.DateTime.Format("20060102"), 10, 32)
			if err != nil {
				return nil, err
			}
			found[uint32(value)] = struct{}{}
		}
		if len(reply.List) < int(klinePageSize) {
			break
		}
	}

	dates := make([]uint32, 0, len(found))
	for date := range found {
		dates = append(dates, date)
	}
	sort.Slice(dates, func(left, right int) bool { return dates[left] > dates[right] })
	if len(dates) > days {
		dates = dates[:days]
	}
	return dates, nil
}

// auctionTransaction 返回当天 09:25 的集合竞价成交。
func auctionTransaction(items []proto.HistoryTransactionData) (proto.HistoryTransactionData, bool) {
	for _, item := range items {
		if item.Time.Hour() == 9 && item.Time.Minute() == 25 {
			return item, true
		}
	}
	return proto.HistoryTransactionData{}, false
}

func formatDate(date uint32) string {
	text := fmt.Sprintf("%08d", date)
	return text
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

func isASCIIDigits(value string) bool {
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return value != ""
}
