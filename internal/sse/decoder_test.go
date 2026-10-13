package sse

import (
	"reflect"
	"testing"
)

func TestSSEFramingDecoder(t *testing.T) {
	for _, tc := range []struct {
		name  string
		lines []string
		want  []Event
	}{
		{"multiline", []string{"event: message_delta", "data: {", "data:\"usage\":{}", "data:}", ""}, []Event{{"message_delta", "{\n\"usage\":{}\n}"}}},
		{"crlf", []string{"event: message_stop\r", "data:{}\r", "\r"}, []Event{{"message_stop", "{}"}}},
		{"optional_space", []string{"data:  keep spaces  ", ""}, []Event{{"", " keep spaces  "}}},
		{"empty_data", []string{"data", "data:next", ""}, []Event{{"", "\nnext"}}},
		{"metadata_only", []string{": keepalive", "id: 1", "retry: 5", "event: old", "", "data:{}", ""}, []Event{{"", "{}"}}},
		{"tail", []string{"event: message_stop", "data:{}"}, []Event{{"message_stop", "{}"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var decoder Decoder
			var got []Event
			for _, line := range tc.lines {
				if event := decoder.FeedLine(line); event != nil {
					got = append(got, *event)
				}
			}
			if event := decoder.Finish(); event != nil {
				got = append(got, *event)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("SSE 事件边界或数据改变：got=%#v want=%#v", got, tc.want)
			}
			if decoder.Finish() != nil {
				t.Fatal("重复收尾不能重发尾事件")
			}
		})
	}
}
