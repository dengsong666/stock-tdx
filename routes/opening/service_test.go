package opening

import (
	"errors"
	"strings"
	"testing"

	"github.com/bensema/gotdx/proto"
)

type fakeClient struct {
	host         string
	items        map[string][]proto.MACTransactionItem
	err          error
	calls        int
	disconnected bool
}

func (client *fakeClient) MACTransactionsWithDate(_ uint8, code string, start uint32, count uint32, _ uint32) ([]proto.MACTransactionItem, error) {
	client.calls++
	if client.disconnected {
		return nil, errors.New("client closed")
	}
	if client.err != nil {
		return nil, client.err
	}
	if start != 0 || count != transactionsLimit {
		return nil, errors.New("unexpected paging")
	}
	return client.items[code], nil
}

func (client *fakeClient) Disconnect() error {
	client.disconnected = true
	return nil
}

// fakeFactory 按主站返回预设行为的新客户端，并记录每次新建的客户端。
type fakeFactory struct {
	errByHost map[string]error
	items     map[string][]proto.MACTransactionItem
	created   []*fakeClient
}

func (factory *fakeFactory) newClient(host string, _ int) macTransactionsClient {
	client := &fakeClient{host: host, items: factory.items, err: factory.errByHost[host]}
	factory.created = append(factory.created, client)
	return client
}

// dayItems 模拟 MAC 分笔的返回顺序：先是收盘前那一页，再是更早的一页，页内时间升序。
func dayItems() []proto.MACTransactionItem {
	return []proto.MACTransactionItem{
		{Time: "14:59:57", Price: 26.1, Vol: 30},
		{Time: "15:00:03", Price: 26.2, Vol: 800},
		{Time: "09:25:03", Price: 25.929999, Vol: 1200, TradeCount: 300},
		{Time: "09:30:00", Price: 0, Vol: 0},
		{Time: "09:30:03", Price: 26.019999, Vol: 450},
		{Time: "09:30:06", Price: 25.98, Vol: 210},
	}
}

func TestOpeningTradeSortsPagesAndPicksFirstContinuous(t *testing.T) {
	trade, ok := openingTrade(dayItems())
	if !ok {
		t.Fatal("expected opening trade")
	}
	if trade.AuctionTime == nil || *trade.AuctionTime != "09:25:03" || trade.AuctionPrice == nil || *trade.AuctionPrice != 25.93 {
		t.Fatalf("unexpected auction %+v", trade)
	}
	// 09:30:00 那条价格为 0 的占位记录要跳过，第一笔是 09:30:03。
	if trade.FirstTime != "09:30:03" || trade.FirstPrice != 26.02 || trade.FirstVol != 450 {
		t.Fatalf("unexpected first trade %+v", trade)
	}
}

func TestOpeningTradeMissingWithoutContinuousTrade(t *testing.T) {
	if _, ok := openingTrade([]proto.MACTransactionItem{{Time: "09:25:00", Price: 10, Vol: 1}}); ok {
		t.Fatal("auction only must be missing")
	}
	if _, ok := openingTrade(nil); ok {
		t.Fatal("empty day must be missing")
	}
}

func TestOpeningTradeWithoutAuctionKeepsNullAuction(t *testing.T) {
	trade, ok := openingTrade([]proto.MACTransactionItem{{Time: "09:30:00", Price: 10.5, Vol: 3}})
	if !ok || trade.AuctionTime != nil || trade.AuctionPrice != nil || trade.FirstPrice != 10.5 {
		t.Fatalf("unexpected trade %+v ok=%v", trade, ok)
	}
}

func TestFetchReportsMissingCodes(t *testing.T) {
	factory := &fakeFactory{items: map[string][]proto.MACTransactionItem{"001202": dayItems()}}
	service := newService([]string{"a"}, 1, factory.newClient)
	result, err := service.Fetch(20210512, []string{"001202", "600519"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := result.Trades["001202"]; !ok || strings.Join(result.MissingCodes, ",") != "600519" {
		t.Fatalf("unexpected result %+v", result)
	}
	if len(factory.created) != 1 || !factory.created[0].disconnected {
		t.Fatalf("one request must reuse one connection and close it: %+v", factory.created)
	}
}

func TestFetchFailsOverWithFreshClient(t *testing.T) {
	factory := &fakeFactory{
		errByHost: map[string]error{"a": errors.New("timeout")},
		items:     map[string][]proto.MACTransactionItem{"001202": dayItems()},
	}
	service := newService([]string{"a", "b"}, 1, factory.newClient)
	result, err := service.Fetch(20210512, []string{"001202"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Trades["001202"].FirstPrice != 26.02 {
		t.Fatalf("unexpected result %+v", result)
	}
	// 失败的客户端必须断开且不再复用，换主站新建一个。
	if len(factory.created) != 2 || factory.created[0].host != "a" || !factory.created[0].disconnected || factory.created[1].host != "b" {
		t.Fatalf("unexpected clients %+v", factory.created)
	}
	if service.preferredHost() != 1 {
		t.Fatalf("preferred host must move to b, got %d", service.preferredHost())
	}
	if _, err := service.Fetch(20210512, []string{"001202"}); err != nil {
		t.Fatal(err)
	}
	if factory.created[2].host != "b" {
		t.Fatalf("next request must start from the last good host, got %s", factory.created[2].host)
	}
}

func TestFetchFailsWhenAllHostsFail(t *testing.T) {
	factory := &fakeFactory{errByHost: map[string]error{"a": errors.New("timeout"), "b": errors.New("reset")}}
	service := newService([]string{"a", "b"}, 1, factory.newClient)
	_, err := service.Fetch(20210512, []string{"001202"})
	if err == nil || !strings.Contains(err.Error(), "code=001202") || !strings.Contains(err.Error(), "host=b") {
		t.Fatalf("expected error naming code and last host, got %v", err)
	}
}

func TestFetchRejectsInvalidCodeAndMissingHosts(t *testing.T) {
	factory := &fakeFactory{}
	if _, err := newService([]string{"a"}, 1, factory.newClient).Fetch(20210512, []string{"12345"}); err == nil {
		t.Fatal("invalid code must fail")
	}
	if _, err := newService(nil, 1, factory.newClient).Fetch(20210512, []string{"001202"}); err == nil {
		t.Fatal("missing hosts must fail")
	}
}
