package modelcatalog

import "time"

// Official holiday notices; weekday peak windows fall on the same UTC and China date.
// 2026: https://www.beijing.gov.cn/zhengce/zhengcefagui/202511/t20251104_4258873.html
// 2027: https://www.gov.cn/zhengce/content/202611/content_7085101.htm
var chineseHolidayRanges = map[int][][2]string{
	2026: {
		{"01-01", "01-03"}, {"02-15", "02-23"}, {"04-04", "04-06"}, {"05-01", "05-05"},
		{"06-19", "06-21"}, {"09-25", "09-27"}, {"10-01", "10-07"},
	},
	2027: {
		{"01-01", "01-03"}, {"02-05", "02-13"}, {"04-03", "04-05"}, {"05-01", "05-05"},
		{"06-09", "06-09"}, {"09-15", "09-15"}, {"10-01", "10-07"},
	},
}

func (p Price) usesChineseHolidayCalendar() bool {
	for _, gap := range p.CoverageGaps {
		if gap.Dimension == "calendar" {
			return true
		}
	}
	return false
}

func (p Price) CalendarKnownAt(at time.Time) bool {
	if !p.usesChineseHolidayCalendar() {
		return true
	}
	_, known := chineseHolidayRanges[at.UTC().Year()]
	return known
}

func (p Price) isHoliday(at time.Time) bool {
	if !p.usesChineseHolidayCalendar() {
		return false
	}
	date := at.UTC().Format("01-02")
	for _, interval := range chineseHolidayRanges[at.UTC().Year()] {
		if date >= interval[0] && date <= interval[1] {
			return true
		}
	}
	return false
}
