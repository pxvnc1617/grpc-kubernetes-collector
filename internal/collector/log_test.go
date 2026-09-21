package collector

import (
	"testing"
	"time"
)

// 로그 수준 추출은 형식이 제각각이라 정규식 하나로 전부 잡을 수 없다.
// 중요한 건 "잡히는 것"보다 "못 잡았을 때 버리지 않는 것"이다.
// 못 잡은 줄을 버리면 어떤 형식을 놓쳤는지 영영 알 수 없다.
func TestExtractLevel(t *testing.T) {
	cases := []struct {
		name string
		msg  string
		want string
	}{
		{"logfmt", `2026-09-21T10:00:00+09:00 level=error msg="payment timeout"`, "error"},
		{"대문자", `LEVEL=WARN buffer high`, "warn"},
		{"warning 은 warn 으로 정규화", `level=warning disk almost full`, "warn"},
		{"콜론 구분", `level: info msg=ok`, "info"},
		{"따옴표", `"level":"debug","msg":"x"`, "debug"},
		{"수준 없음", `just a plain line without any marker`, "unknown"},
		{"유사 단어는 잡지 않음", `levelling=error`, "unknown"},
		{"빈 문자열", ``, "unknown"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := extractLevel(c.msg); got != c.want {
				t.Errorf("extractLevel(%q) = %q, want %q", c.msg, got, c.want)
			}
		})
	}
}

// 쿠버네티스는 Timestamps: true 일 때 RFC3339Nano 시각을 앞에 붙인다.
// 시각이 없는 줄도 버리지 않고 수집 시각으로 대체한다.
func TestSplitTimestamp(t *testing.T) {
	t.Run("시각이 있는 줄", func(t *testing.T) {
		ts, msg := splitTimestamp("2026-09-21T06:00:00.123456789Z level=info msg=ok")
		if msg != "level=info msg=ok" {
			t.Errorf("본문이 잘못 분리됨: %q", msg)
		}
		want := time.Date(2026, 9, 21, 6, 0, 0, 123456789, time.UTC)
		if !ts.Equal(want) {
			t.Errorf("시각 파싱 실패: got %v want %v", ts, want)
		}
	})

	t.Run("시각이 없는 줄도 버리지 않는다", func(t *testing.T) {
		before := time.Now()
		ts, msg := splitTimestamp("plain log line")
		if msg != "plain log line" {
			t.Errorf("본문이 손상됨: %q", msg)
		}
		if ts.Before(before.Add(-time.Second)) {
			t.Error("시각이 없으면 현재 시각으로 대체되어야 함")
		}
	})

	t.Run("공백 없는 한 덩어리", func(t *testing.T) {
		_, msg := splitTimestamp("singleword")
		if msg != "singleword" {
			t.Errorf("본문이 손상됨: %q", msg)
		}
	})
}
