package backup

import (
	"sort"
	"time"
)

func retainedKeys(items []SnapshotInfo, keepHourly, keepDaily, keepWeekly, keepMonthly int) map[string]struct{} {
	sort.Slice(items, func(i, j int) bool {
		return snapshotTime(items[i]).After(snapshotTime(items[j]))
	})

	keep := make(map[string]struct{})
	if len(items) == 0 {
		return keep
	}
	keep[items[0].Key] = struct{}{}

	hourly := make(map[string]struct{})
	daily := make(map[string]struct{})
	weekly := make(map[string]struct{})
	monthly := make(map[string]struct{})

	for _, item := range items {
		ts := snapshotTime(item).UTC()
		if keepHourly > 0 {
			b := ts.Format("2006-01-02T15")
			if len(hourly) < keepHourly {
				if _, ok := hourly[b]; !ok {
					hourly[b] = struct{}{}
					keep[item.Key] = struct{}{}
				}
			}
		}
		if keepDaily > 0 {
			b := ts.Format("2006-01-02")
			if len(daily) < keepDaily {
				if _, ok := daily[b]; !ok {
					daily[b] = struct{}{}
					keep[item.Key] = struct{}{}
				}
			}
		}
		if keepWeekly > 0 {
			year, week := ts.ISOWeek()
			b := ts.Format("2006") + "-" + itoa(year) + "-W" + itoa(week)
			if len(weekly) < keepWeekly {
				if _, ok := weekly[b]; !ok {
					weekly[b] = struct{}{}
					keep[item.Key] = struct{}{}
				}
			}
		}
		if keepMonthly > 0 {
			b := ts.Format("2006-01")
			if len(monthly) < keepMonthly {
				if _, ok := monthly[b]; !ok {
					monthly[b] = struct{}{}
					keep[item.Key] = struct{}{}
				}
			}
		}
	}
	return keep
}

func snapshotTime(item SnapshotInfo) time.Time {
	if item.CreatedAt != nil && !item.CreatedAt.IsZero() {
		return *item.CreatedAt
	}
	if item.LastModified != nil && !item.LastModified.IsZero() {
		return *item.LastModified
	}
	return time.Time{}
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	if v < 0 {
		return "-" + itoa(-v)
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + (v % 10))
		v /= 10
	}
	return string(buf[i:])
}
