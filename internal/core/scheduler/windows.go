package scheduler

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Window is one weekly recurring opening period. Start and End are local "HH:MM"
// strings in the scheduler's configured location; End is exclusive.
type Window struct {
	Dow   time.Weekday
	Start string
	End   string
}

// Windows is a set of opening periods. A time with no covering window is a quiet
// period: the scheduler never claims outside the windows, so quiet hours need no
// separate configuration.
type Windows []Window

type windowJSON struct {
	Dow   string `json:"dow"`
	Start string `json:"start"`
	End   string `json:"end"`
}

// ParseWindows decodes the config JSON array ([{"dow":"mon","start":"08:00",
// "end":"18:00"}, …]). An empty array is valid and means "no opening periods" (nothing
// is ever claimed). A malformed window is a startup error so a typo cannot silently
// stop dialing.
func ParseWindows(raw string) (Windows, error) {
	var decoded []windowJSON
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		return nil, fmt.Errorf("scheduler: parse windows: %w", err)
	}
	windows := make(Windows, 0, len(decoded))
	for i, w := range decoded {
		dow, ok := parseWeekday(w.Dow)
		if !ok {
			return nil, fmt.Errorf("scheduler: windows[%d]: unknown dow %q", i, w.Dow)
		}
		start, err := parseClock(w.Start)
		if err != nil {
			return nil, fmt.Errorf("scheduler: windows[%d].start: %w", i, err)
		}
		end, err := parseClock(w.End)
		if err != nil {
			return nil, fmt.Errorf("scheduler: windows[%d].end: %w", i, err)
		}
		if start >= end {
			return nil, fmt.Errorf("scheduler: windows[%d]: start %s must be before end %s", i, w.Start, w.End)
		}
		windows = append(windows, Window{Dow: dow, Start: w.Start, End: w.End})
	}
	return windows, nil
}

// Allows reports whether now falls inside any window. An empty set allows nothing.
func (ws Windows) Allows(now time.Time, loc *time.Location) bool {
	if loc == nil {
		loc = time.UTC
	}
	local := now.In(loc)
	minute := local.Hour()*60 + local.Minute()
	for _, w := range ws {
		if w.Dow != local.Weekday() {
			continue
		}
		start, startErr := parseClock(w.Start)
		end, endErr := parseClock(w.End)
		if startErr != nil || endErr != nil {
			continue // ParseWindows already rejected malformed windows
		}
		if minute >= start && minute < end {
			return true
		}
	}
	return false
}

// Next returns the earliest instant at or after from that is inside a window. If from is
// already inside a window, from is returned. With no windows it returns from (callers
// that gate claiming on Allows never reach this with an empty set). DST is handled by
// constructing each candidate with time.Date in loc.
func (ws Windows) Next(from time.Time, loc *time.Location) time.Time {
	if loc == nil {
		loc = time.UTC
	}
	local := from.In(loc)
	if ws.Allows(local, loc) {
		return local
	}
	for day := 0; day <= 8; day++ {
		d := local.AddDate(0, 0, day)
		var best time.Time
		for _, w := range ws {
			if w.Dow != d.Weekday() {
				continue
			}
			start, err := parseClock(w.Start)
			if err != nil {
				continue
			}
			candidate := time.Date(d.Year(), d.Month(), d.Day(), start/60, start%60, 0, 0, loc)
			if candidate.Before(local) {
				continue
			}
			if best.IsZero() || candidate.Before(best) {
				best = candidate
			}
		}
		if !best.IsZero() {
			return best
		}
	}
	return local
}

func parseWeekday(raw string) (time.Weekday, bool) {
	switch strings.ToLower(raw) {
	case "sun":
		return time.Sunday, true
	case "mon":
		return time.Monday, true
	case "tue":
		return time.Tuesday, true
	case "wed":
		return time.Wednesday, true
	case "thu":
		return time.Thursday, true
	case "fri":
		return time.Friday, true
	case "sat":
		return time.Saturday, true
	default:
		return 0, false
	}
}

// parseClock converts a strict "HH:MM" string into minutes since midnight.
func parseClock(raw string) (int, error) {
	parts := strings.Split(raw, ":")
	if len(parts) != 2 || len(parts[0]) != 2 || len(parts[1]) != 2 {
		return 0, fmt.Errorf("invalid time %q (want HH:MM)", raw)
	}
	hour, err := strconv.Atoi(parts[0])
	if err != nil || hour < 0 || hour > 23 {
		return 0, fmt.Errorf("invalid time %q (want HH:MM)", raw)
	}
	minute, err := strconv.Atoi(parts[1])
	if err != nil || minute < 0 || minute > 59 {
		return 0, fmt.Errorf("invalid time %q (want HH:MM)", raw)
	}
	return hour*60 + minute, nil
}
