package boards

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeFetcher struct {
	boards  []Board
	members BoardMembers
	err     error
}

func (fetcher *fakeFetcher) FetchBoards() ([]Board, error) {
	return fetcher.boards, fetcher.err
}

func (fetcher *fakeFetcher) FetchMembers(symbol string) (BoardMembers, error) {
	if fetcher.err != nil {
		return BoardMembers{}, fetcher.err
	}
	result := fetcher.members
	result.Code = symbol
	return result, nil
}

func TestServeBoardsReturnsUnifiedResponse(t *testing.T) {
	route := NewHandler(&fakeFetcher{boards: []Board{{Code: "881121", Name: "银行", Type: 0}}})
	request := httptest.NewRequest(http.MethodPost, Path, strings.NewReader(""))
	recorder := httptest.NewRecorder()

	route.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	var payload struct {
		Code int `json:"code"`
		Data struct {
			Boards []Board `json:"boards"`
		} `json:"data"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.Code != 0 || len(payload.Data.Boards) != 1 || payload.Data.Boards[0].Code != "881121" {
		t.Fatalf("unexpected payload: %+v", payload)
	}
}

func TestServeMembersValidatesSymbol(t *testing.T) {
	route := NewHandler(&fakeFetcher{members: BoardMembers{Stocks: []Stock{{Code: "600000", Name: "浦发银行"}}}})
	request := httptest.NewRequest(http.MethodPost, MembersPath, strings.NewReader(`{"symbol":"abc"}`))
	recorder := httptest.NewRecorder()

	route.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
}

func TestServeMembersReturnsStocks(t *testing.T) {
	route := NewHandler(&fakeFetcher{members: BoardMembers{Stocks: []Stock{{Code: "600000", Name: "浦发银行", Market: 1}}}})
	request := httptest.NewRequest(http.MethodPost, MembersPath, strings.NewReader(`{"symbol":"881121"}`))
	recorder := httptest.NewRecorder()

	route.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	var payload struct {
		Data BoardMembers `json:"data"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.Data.Code != "881121" || len(payload.Data.Stocks) != 1 || payload.Data.Stocks[0].Code != "600000" {
		t.Fatalf("unexpected payload: %+v", payload)
	}
}

func TestServeBoardsRejectsGet(t *testing.T) {
	route := NewHandler(&fakeFetcher{})
	request := httptest.NewRequest(http.MethodGet, Path, nil)
	recorder := httptest.NewRecorder()

	route.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", recorder.Code)
	}
}
