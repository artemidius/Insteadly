package scheduler

import (
	"strings"
	"testing"
	"time"
)

func vienna(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Vienna")
	if err != nil {
		t.Fatalf("load Europe/Vienna: %v", err)
	}
	return loc
}

func weekdayWindows() Windows {
	return Windows{
		{Dow: time.Monday, Start: "08:00", End: "18:00"},
		{Dow: time.Tuesday, Start: "08:00", End: "18:00"},
		{Dow: time.Wednesday, Start: "08:00", End: "18:00"},
		{Dow: time.Thursday, Start: "08:00", End: "18:00"},
		{Dow: time.Friday, Start: "08:00", End: "18:00"},
	}
}

func TestParseWindowsDefault(t *testing.T) {
	raw := `[{"dow":"mon","start":"08:00","end":"18:00"},{"dow":"sun","start":"10:00","end":"12:30"}]`
	ws, err := ParseWindows(raw)
	if err != nil {
		t.Fatalf("ParseWindows: %v", err)
	}
	if len(ws) != 2 || ws[0].Dow != time.Monday || ws[1].Dow != time.Sunday {
		t.Fatalf("windows = %+v", ws)
	}
	if ws[1].Start != "10:00" || ws[1].End != "12:30" {
		t.Errorf("second window = %+v", ws[1])
	}
}

func TestParseWindowsErrors(t *testing.T) {
	cases := map[string]string{
		"malformed json": `[{"dow":`,
		"unknown dow":    `[{"dow":"monnday","start":"08:00","end":"18:00"}]`,
		"empty dow":      `[{"start":"08:00","end":"18:00"}]`,
		"bad start":      `[{"dow":"mon","start":"8:00","end":"18:00"}]`,
		"bad end":        `[{"dow":"mon","start":"08:00","end":"24:00"}]`,
		"start >= end":   `[{"dow":"mon","start":"18:00","end":"08:00"}]`,
		"empty window":   `[{"dow":"mon","start":"08:00","end":"08:00"}]`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseWindows(raw); err == nil {
				t.Fatalf("ParseWindows(%s) accepted a malformed window", raw)
			}
		})
	}
	empty, err := ParseWindows(`[]`)
	if err != nil {
		t.Fatalf("empty array: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("empty array = %+v", empty)
	}
}

func TestWindowsAllows(t *testing.T) {
	loc := vienna(t)
	ws := weekdayWindows()
	cases := []struct {
		name string
		when time.Time
		want bool
	}{
		{"inside", time.Date(2026, 11, 17, 9, 0, 0, 0, loc), true}, // Tuesday
		{"at start", time.Date(2026, 11, 17, 8, 0, 0, 0, loc), true},
		{"before", time.Date(2026, 11, 17, 7, 59, 0, 0, loc), false},
		{"at end is exclusive", time.Date(2026, 11, 17, 18, 0, 0, 0, loc), false},
		{"weekend", time.Date(2026, 11, 21, 10, 0, 0, 0, loc), false}, // Saturday
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ws.Allows(tc.when, loc); got != tc.want {
				t.Errorf("Allows(%s) = %v, want %v", tc.when.Format(time.RFC3339), got, tc.want)
			}
		})
	}
	if weekdayWindows().Allows(time.Date(2026, 11, 17, 9, 0, 0, 0, loc), loc) != true {
		t.Error("Tuesday 09:00 should be allowed")
	}
	if (Windows{}).Allows(time.Date(2026, 11, 17, 9, 0, 0, 0, loc), loc) {
		t.Error("an empty window set must allow nothing")
	}
}

func TestWindowsNext(t *testing.T) {
	loc := vienna(t)
	ws := weekdayWindows()
	cases := []struct {
		name string
		from time.Time
		want time.Time
	}{
		{
			name: "inside returns from",
			from: time.Date(2026, 11, 17, 9, 0, 0, 0, loc),
			want: time.Date(2026, 11, 17, 9, 0, 0, 0, loc),
		},
		{
			name: "after close moves to next weekday",
			from: time.Date(2026, 11, 17, 19, 0, 0, 0, loc),
			want: time.Date(2026, 11, 18, 8, 0, 0, 0, loc),
		},
		{
			name: "weekend moves to Monday",
			from: time.Date(2026, 11, 21, 10, 0, 0, 0, loc),
			want: time.Date(2026, 11, 23, 8, 0, 0, 0, loc),
		},
		{
			name: "before open moves to same day",
			from: time.Date(2026, 11, 17, 6, 0, 0, 0, loc),
			want: time.Date(2026, 11, 17, 8, 0, 0, 0, loc),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ws.Next(tc.from, loc)
			if !got.Equal(tc.want) {
				t.Errorf("Next(%s) = %s, want %s", tc.from.Format(time.RFC3339), got.Format(time.RFC3339), tc.want.Format(time.RFC3339))
			}
		})
	}
	if got := (Windows{}).Next(time.Date(2026, 11, 17, 9, 0, 0, 0, loc), loc); !got.Equal(time.Date(2026, 11, 17, 9, 0, 0, 0, loc)) {
		t.Errorf("empty windows Next = %s, want the input", got)
	}
}

func TestWindowsNextAcrossDST(t *testing.T) {
	loc := vienna(t)
	ws := weekdayWindows()

	// 2027-03-28 is the spring-forward Sunday in Europe/Vienna.
	beforeDST := time.Date(2027, 3, 26, 19, 0, 0, 0, loc) // Friday, CET (+01:00)
	got := ws.Next(beforeDST, loc)
	want := time.Date(2027, 3, 29, 8, 0, 0, 0, loc) // Monday, CEST (+02:00)
	if !got.Equal(want) {
		t.Fatalf("Next across spring-forward = %s, want %s", got, want)
	}
	if _, offset := got.Zone(); offset != 2*60*60 {
		t.Errorf("spring-forward offset = %d, want 7200 (CEST)", offset)
	}
	if got.Hour() != 8 || got.Minute() != 0 {
		t.Errorf("local time = %02d:%02d, want 08:00", got.Hour(), got.Minute())
	}

	// 2027-10-31 is the fall-back Sunday.
	beforeFallBack := time.Date(2027, 10, 29, 19, 0, 0, 0, loc) // Friday, CEST
	got = ws.Next(beforeFallBack, loc)
	want = time.Date(2027, 11, 1, 8, 0, 0, 0, loc) // Monday, CET (+01:00)
	if !got.Equal(want) {
		t.Fatalf("Next across fall-back = %s, want %s", got, want)
	}
	if _, offset := got.Zone(); offset != 60*60 {
		t.Errorf("fall-back offset = %d, want 3600 (CET)", offset)
	}
	if got.Hour() != 8 {
		t.Errorf("local hour = %d, want 8", got.Hour())
	}
}

func TestParseWindowsErrorMentionWindow(t *testing.T) {
	_, err := ParseWindows(`[{"dow":"mon","start":"08:00","end":"07:00"}]`)
	if err == nil || !strings.Contains(err.Error(), "windows[0]") {
		t.Fatalf("err = %v, want an indexed windows error", err)
	}
}
