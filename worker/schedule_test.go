package worker

import (
	"strings"
	"testing"
	"time"
)

// TestParseSchedule_Pinned pins the supported schedule surface: 5-field
// Vixie syntax (lists, ranges, steps, names, "?", 7=Sunday, the
// crontab(5) day-of-month/day-of-week OR rule) plus the descriptors.
func TestParseSchedule_Pinned(t *testing.T) {
	cases := []struct {
		name string
		expr string
		now  time.Time
		want time.Time
	}{
		{
			name: "hour step",
			expr: "0 */6 * * *",
			now:  time.Date(2026, 3, 10, 7, 15, 30, 0, time.UTC),
			want: time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC),
		},
		{
			name: "minute list",
			expr: "0,30 9 * * *",
			now:  time.Date(2026, 3, 10, 9, 10, 0, 0, time.UTC),
			want: time.Date(2026, 3, 10, 9, 30, 0, 0, time.UTC),
		},
		{
			name: "hour range wraps to next day",
			expr: "0 9-17 * * *",
			now:  time.Date(2026, 3, 10, 18, 30, 0, 0, time.UTC),
			want: time.Date(2026, 3, 11, 9, 0, 0, 0, time.UTC),
		},
		{
			name: "range with step",
			expr: "15 8-18/2 * * *",
			now:  time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC),
			want: time.Date(2026, 3, 10, 10, 15, 0, 0, time.UTC),
		},
		{
			name: "month name",
			expr: "0 0 1 jan *",
			now:  time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC),
			want: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "weekday name",
			expr: "0 12 * * sat",
			now:  time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC),  // Tuesday
			want: time.Date(2026, 3, 14, 12, 0, 0, 0, time.UTC), // Saturday
		},
		{
			name: "question mark as star",
			expr: "0 0 ? * mon",
			now:  time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC), // Tuesday
			want: time.Date(2026, 3, 16, 0, 0, 0, 0, time.UTC), // Monday
		},
		{
			name: "sunday as 7",
			expr: "0 0 * * 7",
			now:  time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC), // Tuesday
			want: time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC), // Sunday
		},
		{
			name: "dom and dow both restricted fire on either (crontab OR)",
			expr: "0 0 13 * fri",
			now:  time.Date(2026, 3, 14, 0, 30, 0, 0, time.UTC), // Saturday
			want: time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC),  // next Friday, before Apr 13
		},
		{
			name: "step on star counts as restricted for the OR rule",
			expr: "0 0 */2 * 1",
			now:  time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC), // Tuesday
			want: time.Date(2026, 3, 11, 0, 0, 0, 0, time.UTC),  // odd day matches via OR
		},
		{
			name: "exact boundary returns strictly after",
			expr: "30 10 * * *",
			now:  time.Date(2026, 3, 10, 10, 30, 0, 0, time.UTC),
			want: time.Date(2026, 3, 11, 10, 30, 0, 0, time.UTC),
		},
		{
			name: "@hourly",
			expr: "@hourly",
			now:  time.Date(2026, 3, 10, 10, 30, 14, 0, time.UTC),
			want: time.Date(2026, 3, 10, 11, 0, 0, 0, time.UTC),
		},
		{
			name: "@daily",
			expr: "@daily",
			now:  time.Date(2026, 3, 10, 10, 30, 0, 0, time.UTC),
			want: time.Date(2026, 3, 11, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "@midnight",
			expr: "@midnight",
			now:  time.Date(2026, 3, 10, 10, 30, 0, 0, time.UTC),
			want: time.Date(2026, 3, 11, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "@weekly",
			expr: "@weekly",
			now:  time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC), // Tuesday
			want: time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC), // Sunday 00:00
		},
		{
			name: "@monthly",
			expr: "@monthly",
			now:  time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC),
			want: time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "@every keeps sub-minute periods",
			expr: "@every 90s",
			now:  time.Date(2026, 3, 10, 10, 0, 0, 500_000_000, time.UTC),
			want: time.Date(2026, 3, 10, 10, 1, 30, 0, time.UTC),
		},
		{
			name: "@every 5m",
			expr: "@every 5m",
			now:  time.Date(2026, 3, 10, 10, 3, 12, 500_000_000, time.UTC),
			want: time.Date(2026, 3, 10, 10, 8, 12, 0, time.UTC),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			schedule := mustSchedule(t, tc.expr)
			got := schedule.Next(tc.now)
			if !got.Equal(tc.want) {
				t.Fatalf("Next(%s) = %s, want %s", tc.now, got, tc.want)
			}
		})
	}
}

// TestParseSchedule_Rejected pins the trimmed surface: the 6-field seconds
// form, @yearly/@annually, and TZ= prefixes are rejected along with plain
// syntax errors.
func TestParseSchedule_Rejected(t *testing.T) {
	cases := []struct {
		name    string
		expr    string
		errHint string // optional substring the error must carry
	}{
		{name: "empty", expr: ""},
		{name: "garbage", expr: "not a cron"},
		{name: "four fields", expr: "1 2 3 4"},
		{name: "six fields (seconds)", expr: "15 30 * * * *", errHint: "not supported"},
		{name: "@yearly", expr: "@yearly", errHint: "0 0 1 1 *"},
		{name: "@annually", expr: "@annually", errHint: "0 0 1 1 *"},
		{name: "TZ prefix", expr: "TZ=UTC 0 0 * * *", errHint: "timezone"},
		{name: "CRON_TZ prefix", expr: "CRON_TZ=Europe/Istanbul 0 0 * * *", errHint: "timezone"},
		{name: "minute out of bounds", expr: "60 * * * *"},
		{name: "dow out of bounds", expr: "* * * * 8"},
		{name: "zero step", expr: "*/0 * * * *"},
		{name: "bad @every duration", expr: "@every nope"},
		{name: "unknown descriptor", expr: "@fortnightly"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseSchedule(tc.expr)
			if err == nil {
				t.Fatalf("ParseSchedule(%q) = nil error, want rejection", tc.expr)
			}
			if tc.errHint != "" && !strings.Contains(err.Error(), tc.errHint) {
				t.Fatalf("ParseSchedule(%q) error = %q, want substring %q", tc.expr, err.Error(), tc.errHint)
			}
		})
	}
}
