package proto

import (
	"encoding/binary"
	"math"
	"testing"
)

// buildUnusualPayload 按 `v1 + 三段 float32` 拼出一条异动负载，用于校验金额换算。
func buildUnusualPayload(v1 byte, v2, v3, v4 float32) []byte {
	buf := make([]byte, 13)
	buf[0] = v1
	binary.LittleEndian.PutUint32(buf[1:5], math.Float32bits(v2))
	binary.LittleEndian.PutUint32(buf[5:9], math.Float32bits(v3))
	binary.LittleEndian.PutUint32(buf[9:13], math.Float32bits(v4))
	return buf
}

// buildLimitGroupPayload 按 `方向 + 子类型 + 两段 float32` 拼出涨跌停组负载。
//
// 实际调用方固定切 13 字节（`data[base+15:base+28]`），有效字段只占前 10 字节，
// 尾部 3 字节是别的记录的残余，这里补零复现同一形状。
func buildLimitGroupPayload(direction byte, subType byte, price, lots float32) []byte {
	buf := make([]byte, 13)
	buf[0] = direction
	buf[1] = subType
	binary.LittleEndian.PutUint32(buf[2:6], math.Float32bits(price))
	binary.LittleEndian.PutUint32(buf[6:10], math.Float32bits(lots))
	return buf
}

// 单位口径来自 2026-11-27 通达信客户端截图逐条核对：
// 主力买卖/大单/竞价试买的第三段是股数，涨跌停组是封单量（手）。
func TestUnusualAmountUnits(t *testing.T) {
	cases := []struct {
		name      string
		eventType byte
		payload   []byte
		wantDesc  string
		wantAmt   float64
		hasAmt    bool
	}{
		{
			name:      "主力买入：股数 × 价格",
			eventType: 0x03,
			payload:   buildUnusualPayload(0x00, 39.59, 786500, 0),
			wantDesc:  "主力买入",
			wantAmt:   786500 * 39.59,
			hasAmt:    true,
		},
		{
			name:      "主力卖出：股数 × 价格",
			eventType: 0x03,
			payload:   buildUnusualPayload(0x01, 39.55, 888000, 0),
			wantDesc:  "主力卖出",
			wantAmt:   888000 * 39.55,
			hasAmt:    true,
		},
		{
			name:      "大单托盘：价格在第四段",
			eventType: 0x10,
			payload:   buildUnusualPayload(0x00, 0, 359500, 19.0),
			wantDesc:  "大单托盘",
			wantAmt:   359500 * 19.0,
			hasAmt:    true,
		},
		{
			name:      "大单压盘：价格在第二段",
			eventType: 0x11,
			payload:   buildUnusualPayload(0x00, 19.13, 465400, 0),
			wantDesc:  "大单压盘",
			wantAmt:   465400 * 19.13,
			hasAmt:    true,
		},
		{
			name:      "竞价试买：股数 × 价格",
			eventType: 0x13,
			payload:   buildUnusualPayload(0x01, 2.55, 42157, 0),
			wantDesc:  "竞价试买",
			wantAmt:   42157 * 2.55,
			hasAmt:    true,
		},
		{
			name:      "封跌停板：封单量(手) × 价格 × 100",
			eventType: 0x14,
			payload:   buildLimitGroupPayload(0x01, 0x02, 9.24, 330996),
			wantDesc:  "封跌停板",
			wantAmt:   330996 * 9.24 * 100,
			hasAmt:    true,
		},
		{
			name:      "封涨大减：封单量(手) × 价格 × 100",
			eventType: 0x14,
			payload:   buildLimitGroupPayload(0x00, 0x04, 16.17, 48307),
			wantDesc:  "封涨大减",
			wantAmt:   48307 * 16.17 * 100,
			hasAmt:    true,
		},
		{
			name:      "尾盘拉升：单位未确认，不给金额",
			eventType: 0x15,
			payload:   buildUnusualPayload(0x02, 0.0078, 9754, 0),
			wantDesc:  "尾盘拉升",
			wantAmt:   0,
			hasAmt:    false,
		},
		{
			name:      "急速拉升：纯百分比，不给金额",
			eventType: 0x1d,
			payload:   buildUnusualPayload(0x00, 0.0163, 0, 0),
			wantDesc:  "急速拉升",
			wantAmt:   0,
			hasAmt:    false,
		},
	}

	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			got := unpackUnusualByType(item.eventType, item.payload)
			if got.Desc != item.wantDesc {
				t.Fatalf("desc = %q, want %q", got.Desc, item.wantDesc)
			}
			if got.HasAmount != item.hasAmt {
				t.Fatalf("hasAmount = %v, want %v", got.HasAmount, item.hasAmt)
			}
			if !item.hasAmt {
				return
			}
			// float32 入参转 float64 有精度损失，按相对误差 1e-4 判等。
			diff := math.Abs(got.Amount-item.wantAmt) / math.Max(math.Abs(item.wantAmt), 1)
			if diff > 1e-4 {
				t.Fatalf("amount = %.2f, want %.2f (diff %.6f)", got.Amount, item.wantAmt, diff)
			}
		})
	}
}

// 涨跌停组的两段数值取偏移 2:6 / 6:10，与 v2/v3（1:5 / 5:9）不同；
// 这条用例钉住「不要拿 v2/v3 复算封单金额」。
func TestLimitGroupAmountDoesNotUseRawV2V3(t *testing.T) {
	payload := buildLimitGroupPayload(0x00, 0x02, 16.17, 210828)
	got := unpackUnusualByType(0x14, payload)
	if got.Value != "16.17/210828.00" {
		t.Fatalf("value = %q", got.Value)
	}
	macItem := MACMarketMonitorItem{
		V2: float64(math.Float32frombits(binary.LittleEndian.Uint32(payload[1:5]))),
		V3: float64(math.Float32frombits(binary.LittleEndian.Uint32(payload[5:9]))),
	}
	if math.Abs(macItem.V3-210828) < 1 {
		t.Fatalf("raw.V3 竟然等于封单量，说明偏移假设不成立，需要重新核对协议")
	}
	// float32 入参本身只有约 7 位有效数字，3.4 亿的金额允许 1e-6 的相对误差。
	want := 210828.0 * 16.17 * 100
	if math.Abs(got.Amount-want)/want > 1e-6 {
		t.Fatalf("amount = %.2f, want %.2f", got.Amount, want)
	}
}
