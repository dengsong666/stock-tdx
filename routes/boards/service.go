// Package boards 提供板块列表与板块成分股的 HTTP 服务。
package boards

import (
	"fmt"
	"strings"
	"sync"

	gotdx "github.com/bensema/gotdx"
	"github.com/bensema/gotdx/proto"
	"github.com/bensema/gotdx/types"
)

const defaultTimeoutSec = 3

// boardTypes 是采集范围：除 255（全部）以外的全部已知板块类型。
// 按类型分别查询再合并，才能保留类型语义；类型只用于调用方映射分类，不再落库。
var boardTypes = []uint16{
	types.BoardTypeHY,
	types.BoardTypeHY2,
	types.BoardTypeGN,
	types.BoardTypeFG,
	types.BoardTypeDQ,
	types.BoardTypeUnknown,
	types.BoardTypeHYOther,
	types.BoardTypeOther11,
	types.BoardTypeOther12,
}

type boardClient interface {
	MACBoardCount(boardType uint16) (uint16, error)
	MACBoardList(boardType uint16, count uint32) ([]proto.MACBoardListItem, error)
	MACBoardMembers(boardSymbol string, count uint32) ([]proto.MACBoardMemberItem, error)
	Disconnect() error
}

type boardClientFactory func(host string, timeoutSec int) boardClient

// Service 通过独立短连接查询 TDX 板块数据，并在多个 MAC 主站之间故障转移。
type Service struct {
	hosts      []string
	timeoutSec int
	newClient  boardClientFactory

	mu        sync.Mutex
	preferred int
}

// NewService 创建使用内置 MAC 主站列表的生产板块服务。
func NewService() *Service {
	return newService(gotdx.MACHostAddresses(), defaultTimeoutSec, newBoardClient)
}

func newService(hosts []string, timeoutSec int, factory boardClientFactory) *Service {
	return &Service{
		hosts:      append([]string(nil), hosts...),
		timeoutSec: timeoutSec,
		newClient:  factory,
	}
}

func newBoardClient(host string, timeoutSec int) boardClient {
	return gotdx.NewMAC(
		gotdx.WithMacTCPAddress(host),
		gotdx.WithMacTCPAddressPool(),
		gotdx.WithTimeoutSec(timeoutSec),
	)
}

// FetchBoards 拉取全部类型板块；同一板块出现在多个类型时保留首次出现的类型。
func (service *Service) FetchBoards() ([]Board, error) {
	var collected []Board
	err := service.withClient(func(client boardClient) error {
		local := make([]Board, 0, 600)
		seen := make(map[string]struct{}, 600)
		for _, boardType := range boardTypes {
			count, err := client.MACBoardCount(boardType)
			if err != nil {
				return fmt.Errorf("板块类型 %d 数量查询失败: %w", boardType, err)
			}
			if count == 0 {
				continue
			}
			items, err := client.MACBoardList(boardType, uint32(count))
			if err != nil {
				return fmt.Errorf("板块类型 %d 列表查询失败: %w", boardType, err)
			}
			for _, item := range items {
				code := strings.TrimSpace(item.Code)
				name := strings.TrimSpace(item.Name)
				if code == "" || name == "" {
					continue
				}
				if _, ok := seen[code]; ok {
					continue
				}
				seen[code] = struct{}{}
				local = append(local, Board{Code: code, Name: name, Type: boardType})
			}
		}
		collected = local
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(collected) == 0 {
		return nil, fmt.Errorf("TDX 板块列表为空")
	}
	return collected, nil
}

// FetchMembers 拉取指定板块的全部成分股；板块代码由调用方传入（见上方路由说明）。
func (service *Service) FetchMembers(symbol string) (BoardMembers, error) {
	symbol = strings.TrimSpace(symbol)
	if symbol == "" {
		return BoardMembers{}, fmt.Errorf("板块代码不能为空")
	}
	var result BoardMembers
	err := service.withClient(func(client boardClient) error {
		items, err := client.MACBoardMembers(symbol, 0)
		if err != nil {
			return err
		}
		stocks := make([]Stock, 0, len(items))
		for _, item := range items {
			code := strings.TrimSpace(item.Symbol)
			if code == "" {
				continue
			}
			stocks = append(stocks, Stock{
				Code:   code,
				Name:   strings.TrimSpace(item.Name),
				Market: item.Market,
			})
		}
		result = BoardMembers{Code: symbol, Stocks: stocks}
		return nil
	})
	if err != nil {
		return BoardMembers{}, err
	}
	return result, nil
}

// withClient 依次尝试各主站；成功的起始主站会被记住，下次优先使用。
func (service *Service) withClient(fn func(boardClient) error) error {
	if len(service.hosts) == 0 {
		return fmt.Errorf("TDX 板块服务没有可用主站")
	}
	service.mu.Lock()
	start := service.preferred
	service.mu.Unlock()

	var lastErr error
	for offset := 0; offset < len(service.hosts); offset++ {
		index := (start + offset) % len(service.hosts)
		client := service.newClient(service.hosts[index], service.timeoutSec)
		err := fn(client)
		_ = client.Disconnect()
		if err == nil {
			service.mu.Lock()
			service.preferred = index
			service.mu.Unlock()
			return nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("TDX 板块服务没有可用主站")
	}
	return lastErr
}
