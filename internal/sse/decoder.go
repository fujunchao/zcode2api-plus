// Package sse 只负责 SSE 成帧，不解释 JSON、不改写模型协议或传输字节。
package sse

import "strings"

// Event 保存一次空行分隔的事件；Name 来自 event 字段，Data 按规范用换行连接。
type Event struct {
	Name string
	Data string
}

// Decoder 的零值可用。调用方负责分行；LF/CRLF 均支持，EOF 时可调用 Finish。
type Decoder struct {
	name    string
	data    strings.Builder
	hasData bool
}

// FeedLine 输入一行（不含 LF）；只有遇到事件边界才返回完整事件。
func (d *Decoder) FeedLine(line string) *Event {
	line = strings.TrimSuffix(line, "\r")
	if line == "" {
		return d.Finish()
	}
	field, value, _ := strings.Cut(line, ":")
	// SSE 只移除冒号后的一个可选空格，不能任意裁剪数据内容。
	value = strings.TrimPrefix(value, " ")
	switch field {
	case "event":
		d.name = value
	case "data":
		if d.hasData {
			d.data.WriteByte('\n')
		}
		d.data.WriteString(value)
		d.hasData = true
	}
	return nil
}

// Finish 冲刷尾事件并清空缓冲；兼容现有客户端的 EOF 无尾空行形态，重复调用幂等。
func (d *Decoder) Finish() *Event {
	var event *Event
	if d.hasData {
		event = &Event{Name: d.name, Data: d.data.String()}
	}
	d.name, d.data, d.hasData = "", strings.Builder{}, false
	return event
}
