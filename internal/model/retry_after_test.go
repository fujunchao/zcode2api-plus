package model

import (
	"testing"
	"time"
)

func TestInitializationRetryAfterDeadline(t *testing.T) {
	now := time.Unix(1000, 0).UTC()
	for _, tc := range []struct {
		value string
		want  int64
	}{
		{"120", 1120000}, {"Thu, 01 Jan 1970 00:18:40 GMT", 1120000},
		{"Thu, 01 Jan 1970 00:10:00 GMT", 0}, {"-1", 0}, {"", 0}, {"invalid", 0},
		{"9223372036854775807", 9223372036854775807},
	} {
		if got := RetryAfterDeadline(tc.value, now); got != tc.want {
			t.Fatalf("%q: got %d want %d", tc.value, got, tc.want)
		}
	}
}
