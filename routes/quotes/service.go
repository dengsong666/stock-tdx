// Package quotes 提供面向业务调用的 TDX 批量股票行情服务。
package quotes

import (
	"fmt"
	"log"
	"math"
	"strings"
	"sync"

	gotdx "github.com/bensema/gotdx"
	"github.com/bensema/gotdx/proto"
)

const defaultTimeoutSec = 3

// quoteFieldBitmap 只启用昨收、开高低收、更新时间、价格精度和涨停价。
var quoteFieldBitmap = [20]byte{0x1f, 0x00, 0x18, 0x80, 0x01}

// StockQuote 是提供给内部业务服务的统一股票行情结构。
type StockQuote struct {
	Code          string  `json:"code"`
	Price         float64 `json:"price"`
	Change        float64 `json:"chg"`
	High          float64 `json:"high"`
	Low           float64 `json:"low"`
	Open          float64 `json:"open"`
	PreviousClose float64 `json:"prev_close"`
	LimitUpPrice  float64 `json:"limit_up_price"`
	UpdatedAt     string  `json:"updated_at"`
}

// FetchResult 同时返回成功行情和 TDX 未返回的股票代码。
type FetchResult struct {
	Quotes       map[string]StockQuote
	MissingCodes []string
}

type macSymbolQuoteClient interface {
	MACSymbolQuotes(markets []uint8, codes []string, fieldBitmap [20]byte) (*proto.MACSymbolQuotesReply, error)
	Disconnect() error
}

type macClientFactory func(host string, timeoutSec int) macSymbolQuoteClient

// Service 通过独立短连接查询 TDX，并在多个 MAC 主站之间故障转移。
type Service struct {
	hosts      []string
	timeoutSec int
	newClient  macClientFactory

	mu        sync.Mutex
	preferred int
}

// NewService 创建使用内置 MAC 主站列表的生产行情服务。
func NewService() *Service {
	return newService(gotdx.MACHostAddresses(), defaultTimeoutSec, newMACClient)
}

// newService 创建可注入主站和客户端工厂的行情服务，便于离线测试。
func newService(hosts []string, timeoutSec int, factory macClientFactory) *Service {
	return &Service{
		hosts:      append([]string(nil), hosts...),
		timeoutSec: timeoutSec,
		newClient:  factory,
	}
}

// newMACClient 创建只连接指定主站的 TDX MAC 短连接客户端。
func newMACClient(host string, timeoutSec int) macSymbolQuoteClient {
	return gotdx.NewMAC(
		gotdx.WithMacTCPAddress(host),
		gotdx.WithMacTCPAddressPool(),
		gotdx.WithTimeoutSec(timeoutSec),
	)
}

// Fetch 查询一批股票行情；上游成功但缺失的代码单独返回，供调用方继续降级。
func (service *Service) Fetch(codes []string) (FetchResult, error) {
	markets := make([]uint8, 0, len(codes))
	for _, code := range codes {
		market, err := marketForCode(code)
		if err != nil {
			return FetchResult{}, err
		}
		markets = append(markets, market)
	}

	reply, err := service.query(markets, codes)
	if err != nil {
		return FetchResult{}, err
	}

	quotes := make(map[string]StockQuote, len(codes))
	if reply != nil {
		for _, item := range reply.Stocks {
			code := strings.TrimSpace(item.Symbol)
			if code == "" {
				continue
			}
			quotes[code] = stockQuoteFromItem(code, item.Values)
		}
	}

	missing := make([]string, 0)
	for _, code := range codes {
		if _, ok := quotes[code]; !ok {
			missing = append(missing, code)
		}
	}
	return FetchResult{Quotes: quotes, MissingCodes: missing}, nil
}

// query 从上次成功主站开始尝试，每次失败都使用全新的短连接切换下一主站。
func (service *Service) query(markets []uint8, codes []string) (*proto.MACSymbolQuotesReply, error) {
	if len(service.hosts) == 0 || service.newClient == nil {
		return nil, fmt.Errorf("没有配置 MAC 行情主站")
	}

	preferred := service.preferredHost()
	var lastErr error
	for offset := range service.hosts {
		index := (preferred + offset) % len(service.hosts)
		host := service.hosts[index]
		client := service.newClient(host, service.timeoutSec)
		reply, err := client.MACSymbolQuotes(markets, codes, quoteFieldBitmap)
		_ = client.Disconnect()
		if err == nil {
			service.setPreferredHost(index)
			return reply, nil
		}
		lastErr = fmt.Errorf("host=%s: %w", host, err)
		log.Printf("TDX 批量行情主站失败 host=%s: %v", host, err)
	}

	return nil, lastErr
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

// marketForCode 根据六位证券代码识别深市、沪市或北交所市场号。
func marketForCode(code string) (uint8, error) {
	if len(code) != 6 || !isASCIIDigits(code) {
		return 0, fmt.Errorf("无效股票代码: %s", code)
	}
	if strings.HasPrefix(code, "4") || strings.HasPrefix(code, "8") || strings.HasPrefix(code, "92") {
		return 2, nil
	}
	if strings.HasPrefix(code, "5") || strings.HasPrefix(code, "6") || strings.HasPrefix(code, "9") {
		return 1, nil
	}
	if strings.HasPrefix(code, "0") || strings.HasPrefix(code, "1") || strings.HasPrefix(code, "2") || strings.HasPrefix(code, "3") {
		return 0, nil
	}
	return 0, fmt.Errorf("无法识别股票市场: %s", code)
}

// stockQuoteFromItem 将动态字段值转换成稳定的业务行情结构。
func stockQuoteFromItem(code string, values map[string]any) StockQuote {
	decimalPoint := int(numberValue(values["decimal_point"]))
	if decimalPoint < 0 || decimalPoint > 6 {
		decimalPoint = 2
	}
	previousClose := roundTo(numberValue(values["pre_close"]), decimalPoint)
	price := roundTo(numberValue(values["close"]), decimalPoint)
	limitUpPrice := roundTo(numberValue(values["buy_price_limit"]), decimalPoint)
	if limitUpPrice < 0 {
		limitUpPrice = 0
	}
	change := 0.0
	if previousClose > 0 {
		change = roundTo((price-previousClose)/previousClose*100, 2)
	}

	return StockQuote{
		Code:          code,
		Price:         price,
		Change:        change,
		High:          roundTo(numberValue(values["high"]), decimalPoint),
		Low:           roundTo(numberValue(values["low"]), decimalPoint),
		Open:          roundTo(numberValue(values["open"]), decimalPoint),
		PreviousClose: previousClose,
		LimitUpPrice:  limitUpPrice,
		UpdatedAt:     formatUpdatedAt(uint32Value(values["server_update_date"]), uint32Value(values["server_update_time"])),
	}
}

// numberValue 将协议动态字段中的常见数值类型统一转换为 float64。
func numberValue(value any) float64 {
	switch typed := value.(type) {
	case float64:
		return typed
	case float32:
		return float64(typed)
	case int:
		return float64(typed)
	case int32:
		return float64(typed)
	case int64:
		return float64(typed)
	case uint:
		return float64(typed)
	case uint32:
		return float64(typed)
	case uint64:
		return float64(typed)
	default:
		return 0
	}
}

// uint32Value 将协议动态字段转换为更新时间使用的 uint32。
func uint32Value(value any) uint32 {
	number := numberValue(value)
	if number <= 0 || number > math.MaxUint32 {
		return 0
	}
	return uint32(number)
}

// roundTo 按指定小数位对协议浮点数进行规范化。
func roundTo(value float64, digits int) float64 {
	factor := math.Pow10(digits)
	return math.Round(value*factor) / factor
}

// formatUpdatedAt 将 TDX 日期和时间字段格式化为北京时间文本。
func formatUpdatedAt(date uint32, clock uint32) string {
	if date == 0 || clock == 0 {
		return ""
	}
	dateText := fmt.Sprintf("%08d", date)
	timeText := fmt.Sprintf("%06d", clock)
	return fmt.Sprintf(
		"%s-%s-%s %s:%s:%s",
		dateText[0:4], dateText[4:6], dateText[6:8],
		timeText[0:2], timeText[2:4], timeText[4:6],
	)
}

// isASCIIDigits 判断文本是否完全由 ASCII 数字组成。
func isASCIIDigits(value string) bool {
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return value != ""
}
