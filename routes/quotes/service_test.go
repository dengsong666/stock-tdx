package quotes

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/bensema/gotdx/proto"
)

type fakeMACClient struct {
	reply        *proto.MACSymbolQuotesReply
	err          error
	onQuery      func(markets []uint8, codes []string, bitmap [20]byte)
	disconnected *int
}

// MACSymbolQuotes 返回预设协议结果并记录查询参数。
func (client *fakeMACClient) MACSymbolQuotes(markets []uint8, codes []string, bitmap [20]byte) (*proto.MACSymbolQuotesReply, error) {
	if client.onQuery != nil {
		client.onQuery(markets, codes, bitmap)
	}
	return client.reply, client.err
}

// Disconnect 记录短连接已在每次尝试后关闭。
func (client *fakeMACClient) Disconnect() error {
	if client.disconnected != nil {
		(*client.disconnected)++
	}
	return nil
}

// TestServiceFetchAndHostFailover 验证市场识别、字段转换、主站切换和优先主站记忆。
func TestServiceFetchAndHostFailover(t *testing.T) {
	attempts := make([]string, 0)
	disconnected := 0
	reply := &proto.MACSymbolQuotesReply{Stocks: []proto.MACSymbolQuoteItem{
		{
			Symbol: "600519",
			Values: map[string]any{
				"pre_close":          float64(1292.01),
				"open":               float64(1305),
				"high":               float64(1309.21),
				"low":                float64(1286.2),
				"close":              float64(1297.41),
				"decimal_point":      uint32(2),
				"buy_price_limit":    float64(1421.21),
				"server_update_date": uint32(20260724),
				"server_update_time": uint32(153057),
			},
		},
		{
			Symbol: "920799",
			Values: map[string]any{
				"pre_close":       float32(28.46),
				"open":            float32(27.8),
				"high":            float32(28.26),
				"low":             float32(27.03),
				"close":           float32(27.03),
				"decimal_point":   uint32(2),
				"buy_price_limit": float32(36.99),
			},
		},
	}}
	var gotMarkets []uint8
	var gotCodes []string
	var gotBitmap [20]byte
	factory := func(host string, _ int) macSymbolQuoteClient {
		attempts = append(attempts, host)
		client := &fakeMACClient{disconnected: &disconnected}
		if host == "host-1" {
			client.err = errors.New("timeout")
			return client
		}
		client.reply = reply
		client.onQuery = func(markets []uint8, codes []string, bitmap [20]byte) {
			gotMarkets = append([]uint8(nil), markets...)
			gotCodes = append([]string(nil), codes...)
			gotBitmap = bitmap
		}
		return client
	}
	service := newService([]string{"host-1", "host-2"}, 3, factory)

	result, err := service.Fetch([]string{"600519", "300750", "920799"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotMarkets, []uint8{1, 0, 2}) {
		t.Fatalf("unexpected markets: %#v", gotMarkets)
	}
	if !reflect.DeepEqual(gotCodes, []string{"600519", "300750", "920799"}) {
		t.Fatalf("unexpected codes: %#v", gotCodes)
	}
	if gotBitmap != quoteFieldBitmap {
		t.Fatalf("unexpected bitmap: %x", gotBitmap)
	}
	quote := result.Quotes["600519"]
	if quote.Price != 1297.41 || quote.Change != 0.42 || quote.LimitUpPrice != 1421.21 {
		t.Fatalf("unexpected quote: %#v", quote)
	}
	if quote.UpdatedAt != "2026-07-24 15:30:57" {
		t.Fatalf("unexpected updated_at: %q", quote.UpdatedAt)
	}
	if !reflect.DeepEqual(result.MissingCodes, []string{"300750"}) {
		t.Fatalf("unexpected missing codes: %#v", result.MissingCodes)
	}
	if strings.Join(attempts, ",") != "host-1,host-2" || disconnected != 2 {
		t.Fatalf("unexpected attempts=%v disconnected=%d", attempts, disconnected)
	}

	if _, err := service.Fetch([]string{"600519"}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(attempts, ",") != "host-1,host-2,host-2" {
		t.Fatalf("successful host should become preferred: %v", attempts)
	}
}

// TestServiceReturnsLastHostError 验证全部主站失败时返回最后一个带主站上下文的错误。
func TestServiceReturnsLastHostError(t *testing.T) {
	service := newService([]string{"host-1", "host-2"}, 3, func(host string, _ int) macSymbolQuoteClient {
		return &fakeMACClient{err: errors.New(host + " failed")}
	})
	_, err := service.Fetch([]string{"600519"})
	if err == nil || !strings.Contains(err.Error(), "host=host-2") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestMarketForCode 验证沪深北市场代码识别和非法输入拒绝。
func TestMarketForCode(t *testing.T) {
	tests := map[string]uint8{
		"600519": 1,
		"688001": 1,
		"000001": 0,
		"300750": 0,
		"920799": 2,
		"838275": 2,
	}
	for code, want := range tests {
		got, err := marketForCode(code)
		if err != nil || got != want {
			t.Fatalf("code=%s got=%d want=%d err=%v", code, got, want, err)
		}
	}
	if _, err := marketForCode("ABC"); err == nil {
		t.Fatal("expected invalid code error")
	}
}
