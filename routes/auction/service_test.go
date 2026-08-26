package auction

import (
	"testing"
	"time"

	"github.com/bensema/gotdx/proto"
	"github.com/bensema/gotdx/types"
)

type klineCall struct {
	category uint16
	market   uint8
	code     string
	start    uint16
	count    uint16
	times    uint16
	adjust   uint16
}

type fakeAuctionClient struct {
	bars         map[string][]proto.SecurityBar
	transactions map[string]map[uint32][]proto.HistoryTransactionData
	calls        []klineCall
	disconnected bool
}

func (client *fakeAuctionClient) Connect() (*proto.Hello1Reply, error) {
	return &proto.Hello1Reply{}, nil
}

func (client *fakeAuctionClient) GetKLine(category uint16, market uint8, code string, start uint16, count uint16, times uint16, adjust uint16) (*proto.GetSecurityBarsReply, error) {
	client.calls = append(client.calls, klineCall{category: category, market: market, code: code, start: start, count: count, times: times, adjust: adjust})
	items := client.bars[code]
	if int(start) >= len(items) {
		return &proto.GetSecurityBarsReply{}, nil
	}
	end := int(start) + int(count)
	if end > len(items) {
		end = len(items)
	}
	page := append([]proto.SecurityBar(nil), items[int(start):end]...)
	return &proto.GetSecurityBarsReply{Count: uint16(len(page)), List: page}, nil
}

func (client *fakeAuctionClient) StockHistoryFullTransaction(date uint32, _ uint8, code string) ([]proto.HistoryTransactionData, error) {
	return append([]proto.HistoryTransactionData(nil), client.transactions[code][date]...), nil
}

func (client *fakeAuctionClient) Disconnect() error {
	client.disconnected = true
	return nil
}

func TestServiceFetchRoundsWanAndFindsRecentDates(t *testing.T) {
	location := time.FixedZone("Asia/Shanghai", 8*60*60)
	client := &fakeAuctionClient{
		bars: map[string][]proto.SecurityBar{
			"600127": {
				{DateTime: time.Date(2026, 8, 26, 15, 0, 0, 0, location)},
				{DateTime: time.Date(2026, 8, 26, 9, 31, 0, 0, location)},
				{DateTime: time.Date(2026, 8, 25, 9, 31, 0, 0, location)},
			},
			"000001": {{DateTime: time.Date(2026, 8, 26, 9, 31, 0, 0, location)}},
		},
		transactions: map[string]map[uint32][]proto.HistoryTransactionData{
			"600127": {
				20260826: {{Time: time.Date(2026, 8, 26, 9, 25, 0, 0, location), Price: 9.16, Vol: 122198}},
				20260825: {{Time: time.Date(2026, 8, 25, 9, 25, 0, 0, location), Price: 8.46, Vol: 104192}},
			},
			"000001": {
				20260826: {{Time: time.Date(2026, 8, 26, 9, 30, 0, 0, location), Price: 10, Vol: 100}},
			},
		},
	}
	service := newService(func() auctionClient { return client })
	result, err := service.Fetch([]string{"600127", "000001"}, 2)
	if err != nil {
		t.Fatal(err)
	}
	got := result.Auctions["600127"]
	if len(got) != 2 || got[0].Date != "20260826" || got[0].AmountWan != 11193 || got[1].Date != "20260825" || got[1].AmountWan != 8815 {
		t.Fatalf("unexpected amounts: %#v", got)
	}
	if len(result.Auctions["000001"]) != 0 || len(result.MissingCodes) != 1 || result.MissingCodes[0] != "000001" {
		t.Fatalf("unexpected missing result: %#v", result)
	}
	if !client.disconnected {
		t.Fatal("client must be disconnected")
	}
	for _, call := range client.calls {
		if call.category != types.KLINE_TYPE_1MIN || call.count != klinePageSize || call.times != 1 || call.adjust != types.AdjustNone {
			t.Fatalf("unexpected kline call: %#v", call)
		}
	}
}

func TestMarketForCode(t *testing.T) {
	for code, want := range map[string]uint8{"600127": 1, "300750": 0, "920001": 2} {
		got, err := marketForCode(code)
		if err != nil || got != want {
			t.Fatalf("code=%s got=%d want=%d err=%v", code, got, want, err)
		}
	}
	for _, code := range []string{"510300", "ABCDEF", "60012"} {
		if _, err := marketForCode(code); err == nil {
			t.Fatalf("expected %s to be rejected", code)
		}
	}
}
