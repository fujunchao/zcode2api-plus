package model

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// RetryAfterDeadline 将秒数或 HTTP 日期转成毫秒时间点，超大等待不允许整数溢出成过去。
func RetryAfterDeadline(raw string, now time.Time) int64 {
	raw = strings.TrimSpace(raw)
	if seconds, err := strconv.ParseInt(raw, 10, 64); err == nil {
		if seconds <= 0 {
			return 0
		}
		const limit = int64(1<<63 - 1)
		if seconds > (limit-now.UnixMilli())/1000 {
			return limit
		}
		return now.UnixMilli() + seconds*1000
	}
	if at, err := http.ParseTime(raw); err == nil && at.After(now) {
		return at.UnixMilli()
	}
	return 0
}
