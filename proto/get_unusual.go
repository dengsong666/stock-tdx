package proto

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"
)

// GetUnusual 表示一次获取主力监控事件明细协议的完整流程对象。
//
// 监控事件覆盖盘中异动、大单、涨跌停等类型。零值不可直接使用，必须通过
// NewGetUnusual 构造；对象绑定单次查询流程，不支持多协程并发复用。
type GetUnusual struct {
	reqHeader  *ReqHeader
	respHeader *RespHeader
	request    *GetUnusualRequest
	reply      *GetUnusualReply
}

// GetUnusualRequest 是 GetUnusual 异动监控列表查询使用的请求参数。
type GetUnusualRequest struct {
	Market uint16 // 市场代码，`uint16`。
	Start  uint32 // 起始偏移，`uint32`。
	Count  uint32 // 请求条数，`uint32`；为 0 时默认拉取 600 条。
}

// GetUnusualReply 是 GetUnusual 异动监控列表查询解析完成后的响应数据。
type GetUnusualReply struct {
	Count uint16        // 返回条数，`uint16`。
	List  []UnusualData // 异动事件明细列表，长度与 `Count` 一致。
}

// UnusualData 表示一条主力监控异动事件，包含证券、时间、类型与展示描述。
type UnusualData struct {
	Index       uint16  // 事件序号，2 字节`uint16`。
	Market      uint16  // 市场代码，2 字节`uint16`。
	Code        string  // 6 字节定长证券代码，按 `GBK` 还原。
	Time        string  // 异动时间，格式为时:分:秒，由小时与打包的分秒拆分。
	Desc        string  // 异动描述中文文字，按事件类型映射得到。
	Value       string  // 异动展示数值文字，按事件类型格式化得到。
	Amount      float64 // 金额（元）；HasAmount 为 false 时无意义。
	HasAmount   bool    // 该类型是否带金额（尾盘组与纯百分比类不带）。
	UnusualType uint8   // 异动类型原始编码，1 字节无符号整数。
}

// NewGetUnusual 构造主力监控异动查询对象。
//
// 参数 `req` 指定市场 `Market`、起始偏移 `Start` 与请求条数 `Count`；`Count` 为 0
// 时默认拉取 600 条；`req` 为 `nil` 时使用空请求参数。返回的对象可直接调用
// `BuildRequest` 与 `ParseResponse`。
func NewGetUnusual(req *GetUnusualRequest) *GetUnusual {
	obj := new(GetUnusual)
	obj.reqHeader = new(ReqHeader)
	obj.respHeader = new(RespHeader)
	obj.request = new(GetUnusualRequest)
	obj.reply = new(GetUnusualReply)

	obj.reqHeader.Zip = 0x0c
	obj.reqHeader.SeqID = seqID()
	obj.reqHeader.PacketType = 0x00
	obj.reqHeader.Method = KMSG_UNUSUAL
	if req != nil {
		obj.applyRequest(req)
	}
	return obj
}

func (obj *GetUnusual) applyRequest(req *GetUnusualRequest) {
	// 未指定条数时默认拉取 600 条监控事件。
	if req.Count == 0 {
		req.Count = 600
	}
	obj.request = req
}

// BuildRequest 将请求头与请求参数编码为完整协议报文。
//
// 返回的字节切片可直接写入连接；`PkgLen1` 与 `PkgLen2` 双写为同一负载长度，
// 缓冲区写入失败时返回错误。
func (obj *GetUnusual) BuildRequest() ([]byte, error) {
	// 包头长度双写为 0x0c，请求参数由整体写出。
	obj.reqHeader.PkgLen1 = 0x0c
	obj.reqHeader.PkgLen2 = 0x0c

	buf := new(bytes.Buffer)
	err := binary.Write(buf, binary.LittleEndian, obj.reqHeader)
	err = binary.Write(buf, binary.LittleEndian, obj.request)
	return buf.Bytes(), err
}

// ParseResponse 按固定条目长度解析服务端返回的异动事件报文并填充响应对象。
//
// 参数 `header` 为服务端响应头，`data` 为响应负载字节；记录越界时返回
// `io.ErrUnexpectedEOF`。
func (obj *GetUnusual) ParseResponse(header *RespHeader, data []byte) error {
	obj.respHeader = header

	// 头部 2 字节：事件条数。
	if err := binary.Read(bytes.NewBuffer(data[:2]), binary.LittleEndian, &obj.reply.Count); err != nil {
		return err
	}

	for i := uint16(0); i < obj.reply.Count; i++ {
		// 第 i 条记录起始位置：跳过头部 2 字节后每条定长 32 字节。
		base := int(i)*32 + 2
		if base+32 > len(data) {
			return io.ErrUnexpectedEOF
		}

		// 偏移 0：2 字节市场代码。
		market := binary.LittleEndian.Uint16(data[base : base+2])
		// 偏移 2：6 字节定长证券代码，按 `GBK` 还原。
		code := Utf8ToGbk(data[base+2 : base+8])
		// 偏移 9：1 字节事件类型原始编码。
		eventType := data[base+9]
		// 偏移 11：2 字节事件序号。
		index := binary.LittleEndian.Uint16(data[base+11 : base+13])
		// 偏移 15 起 13 字节类型负载，按事件类型码解析出中文描述、展示数值与金额。
		decoded := unpackUnusualByType(eventType, data[base+15:base+28])
		// 小时存于；分钟与秒打包在的无符号短整数中，按整除 100 与取余 100 拆分。
		hour := int(data[base+29])
		minuteSec := int(binary.LittleEndian.Uint16(data[base+30 : base+32]))

		obj.reply.List = append(obj.reply.List, UnusualData{
			Index:       index,
			Market:      market,
			Code:        code,
			Time:        fmt.Sprintf("%02d:%02d:%02d", hour, minuteSec/100, minuteSec%100),
			Desc:        decoded.Desc,
			Value:       decoded.Value,
			Amount:      decoded.Amount,
			HasAmount:   decoded.HasAmount,
			UnusualType: eventType,
		})
	}

	return nil
}

// Response 返回已解析完成的异动监控响应数据；未调用 `ParseResponse` 时返回空响应对象。
func (obj *GetUnusual) Response() *GetUnusualReply {
	return obj.reply
}

// unusualDecoded 是一条异动事件按类型解码后的展示数据。
//
// `Amount` 是**成交/挂单金额（元）**，只对带资金含义的类型有效；`HasAmount` 为 false 时无意义。
// 各类型的原始单位并不相同（2026-11-27 用通达信客户端截图逐条核对）：
//   - 主力买卖、大单托盘/压盘、竞价试买：第三段是**股数**，金额 = 股数 × 价格；
//   - 涨跌停组（封/打开/逼近/大减）：第三段是**封单量（手）**，金额 = 手数 × 价格 × 100；
//   - 尾盘组（0x15）：单位无法确认（疑似万元），**不计算金额**，避免给出错数。
//
// ⚠ 涨跌停组的两个数值段取的是偏移 2:6 与 6:10，与 v2/v3（偏移 1:5、5:9）不同，
// 所以金额只能在这里算，调用方不要拿 v2/v3 复算。
type unusualDecoded struct {
	Desc      string
	Value     string
	Amount    float64
	HasAmount bool
}

// moneyDecoded 组装「描述 + 展示文本 + 金额（元）」三件套。
func moneyDecoded(desc, value string, amount float64) unusualDecoded {
	return unusualDecoded{Desc: desc, Value: value, Amount: amount, HasAmount: true}
}

// unpackUnusualByType 按事件类型码解析类型负载，返回中文描述、展示数值与金额（元）。
//
// 参数 `eventType` 为事件类型原始编码，`data` 为该事件对应的类型负载字节；
// 无法识别的类型或负载长度不足时返回零值结构体。
func unpackUnusualByType(eventType byte, data []byte) unusualDecoded {
	// 负载不足 13 字节时无法取齐四个字段，安全返回空。
	if len(data) < 13 {
		return unusualDecoded{}
	}

	// 首字节为方向或子类型标志。
	v1 := data[0]
	// 其余三段为 4 字节的单精度浮点数。
	v2 := math.Float32frombits(binary.LittleEndian.Uint32(data[1:5]))
	v3 := math.Float32frombits(binary.LittleEndian.Uint32(data[5:9]))
	v4 := math.Float32frombits(binary.LittleEndian.Uint32(data[9:13]))

	switch eventType {
	case 0x03:
		// 主力买卖：第二段成交价、第三段成交股数。
		value := fmt.Sprintf("%.2f/%.2f", v2, v3)
		amount := float64(v3) * float64(v2)
		if v1 == 0x00 {
			return moneyDecoded("主力买入", value, amount)
		}
		return moneyDecoded("主力卖出", value, amount)
	case 0x04:
		return unusualDecoded{Desc: "加速拉升", Value: fmt.Sprintf("%.2f%%", v2*100)}
	case 0x05:
		return unusualDecoded{Desc: "加速下跌"}
	case 0x06:
		return unusualDecoded{Desc: "低位反弹", Value: fmt.Sprintf("%.2f%%", v2*100)}
	case 0x07:
		return unusualDecoded{Desc: "高位回落", Value: fmt.Sprintf("%.2f%%", v2*100)}
	case 0x08:
		return unusualDecoded{Desc: "撑杆跳高", Value: fmt.Sprintf("%.2f%%", v2*100)}
	case 0x09:
		return unusualDecoded{Desc: "平台跳水", Value: fmt.Sprintf("%.2f%%", v2*100)}
	case 0x0a:
		if v2 < 0 {
			return unusualDecoded{Desc: "单笔冲跌", Value: fmt.Sprintf("%.2f%%", v2*100)}
		}
		return unusualDecoded{Desc: "单笔冲涨", Value: fmt.Sprintf("%.2f%%", v2*100)}
	case 0x0b:
		if v3 == 0 {
			return unusualDecoded{Desc: "区间放量平", Value: fmt.Sprintf("%.1f倍", v2)}
		}
		if v3 < 0 {
			return unusualDecoded{Desc: "区间放量跌", Value: fmt.Sprintf("%.1f倍%.2f%%", v2, v3*100)}
		}
		return unusualDecoded{Desc: "区间放量涨", Value: fmt.Sprintf("%.1f倍%.2f%%", v2, v3*100)}
	case 0x0c:
		return unusualDecoded{Desc: "区间缩量"}
	case 0x10:
		// 大单托盘：价格在第四段、股数在第三段（顺序与其它类型相反）。
		return moneyDecoded("大单托盘", fmt.Sprintf("%.2f/%.2f", v4, v3), float64(v3)*float64(v4))
	case 0x11:
		return moneyDecoded("大单压盘", fmt.Sprintf("%.2f/%.2f", v2, v3), float64(v3)*float64(v2))
	case 0x12:
		return unusualDecoded{Desc: "大单锁盘"}
	case 0x13:
		return moneyDecoded("竞价试买", fmt.Sprintf("%.2f/%.2f", v2, v3), float64(v3)*float64(v2))
	case 0x14:
		// 涨跌停类事件需要更短的子负载（10 字节），长度不足直接返回空。
		if len(data) < 10 {
			return unusualDecoded{}
		}
		// 子类型码位于负载偏移 1；后两段为偏移 2 与 6 起的 4 字节单精度浮点数。
		subType := data[1]
		uv2 := math.Float32frombits(binary.LittleEndian.Uint32(data[2:6]))
		uv3 := math.Float32frombits(binary.LittleEndian.Uint32(data[6:10]))
		// 首字节非零表示下跌方向，决定文案中涨与跌的措辞。
		direction := "涨"
		if v1 != 0x00 {
			direction = "跌"
		}
		desc := ""
		switch subType {
		case 0x01:
			desc = "逼近" + direction + "停"
		case 0x02:
			desc = "封" + direction + "停板"
		case 0x04:
			desc = "封" + direction + "大减"
		case 0x05:
			desc = "打开" + direction + "停"
		}
		value := fmt.Sprintf("%.2f/%.2f", uv2, uv3)
		// 子类型不认识时只给展示文本，不猜封单金额（新子类型的字段布局未必一致）。
		if desc == "" {
			return unusualDecoded{Value: value}
		}
		// 第三段是封单量（手），一手 100 股。
		return moneyDecoded(desc, value, float64(uv3)*float64(uv2)*100)
	case 0x15:
		// 尾盘组：单位无法确认，只给文案与原文，不算金额。
		desc := "尾盘打压"
		switch v1 {
		case 0x00:
			desc = "尾盘??"
		case 0x01:
			desc = "尾盘对倒"
		case 0x02:
			desc = "尾盘拉升"
		}
		return unusualDecoded{Desc: desc, Value: fmt.Sprintf("%.2f%%/%.2f", v2*100, v3)}
	case 0x16:
		if v2 < 0 {
			return unusualDecoded{Desc: "盘中弱势", Value: fmt.Sprintf("%.2f%%", v2*100)}
		}
		return unusualDecoded{Desc: "盘中强势", Value: fmt.Sprintf("%.2f%%", v2*100)}
	case 0x1d:
		return unusualDecoded{Desc: "急速拉升", Value: fmt.Sprintf("%.2f%%", v2*100)}
	case 0x1e:
		return unusualDecoded{Desc: "急速下跌", Value: fmt.Sprintf("%.2f%%", v2*100)}
	default:
		return unusualDecoded{}
	}
}
