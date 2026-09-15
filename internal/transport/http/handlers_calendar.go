package http

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"nexusmail/internal/domain"

	"github.com/gin-gonic/gin"
)

// messageCalendar serves one message as a VEVENT. Safari on macOS claims
// text/calendar and opens Calendar with the event pre-filled, which is the
// "add to calendar" path the menu wants — no download-and-double-click.
func (s *Server) messageCalendar(c *gin.Context) {
	id, ok := idParam(c, "id")
	if !ok {
		return
	}
	message, _, err := s.messages.Get(c.Request.Context(), id)
	if err != nil {
		writeError(c, err)
		return
	}
	when, ok := calendarWhen(message)
	if !ok {
		fail(c, 400, "calendar_no_date", "could not find a date in the message", nil)
		return
	}
	c.Header("Content-Type", "text/calendar; charset=utf-8")
	// inline (not attachment) is what lets the browser hand the type to the OS
	// calendar handler instead of forcing a save dialog.
	c.Header("Content-Disposition", `inline; filename="nexusmail-event.ics"`)
	c.Header("Cache-Control", "no-store")
	c.String(200, buildEventICS(message, when))
}

var (
	// year-first: 2026-03-12 / 2026年3月12日 14:30
	reYearFirst = regexp.MustCompile(`(\d{4})[-/年.](\d{1,2})[-/月.](\d{1,2})[日号]?\s*(\d{1,2})?:?(\d{2})?`)
	// month-first: 3月12日 14:30 / 3/12
	reMonthFirst = regexp.MustCompile(`(\d{1,2})[-/月.](\d{1,2})[日号]?\s*(\d{1,2})?:?(\d{2})?`)
)

func calendarWhen(message domain.Message) (time.Time, bool) {
	text := message.Subject + "\n" + message.BodyText
	if when, ok := parseCalendarTime(text); ok {
		return when, true
	}
	if message.ReceivedAt > 0 {
		return time.UnixMilli(message.ReceivedAt), true
	}
	return time.Time{}, false
}

func parseCalendarTime(text string) (time.Time, bool) {
	now := time.Now()
	if m := reYearFirst.FindStringSubmatch(text); m != nil {
		year, _ := strconv.Atoi(m[1])
		month, _ := strconv.Atoi(m[2])
		day, _ := strconv.Atoi(m[3])
		hour, minute := 9, 0
		if m[4] != "" {
			hour, _ = strconv.Atoi(m[4])
		}
		if m[5] != "" {
			minute, _ = strconv.Atoi(m[5])
		}
		if validClock(year, month, day, hour, minute) {
			return time.Date(year, time.Month(month), day, hour, minute, 0, 0, time.Local), true
		}
	}
	if m := reMonthFirst.FindStringSubmatch(text); m != nil {
		month, _ := strconv.Atoi(m[1])
		day, _ := strconv.Atoi(m[2])
		hour, minute := 9, 0
		if m[3] != "" {
			hour, _ = strconv.Atoi(m[3])
		}
		if m[4] != "" {
			minute, _ = strconv.Atoi(m[4])
		}
		if validClock(now.Year(), month, day, hour, minute) {
			return time.Date(now.Year(), time.Month(month), day, hour, minute, 0, 0, time.Local), true
		}
	}
	return time.Time{}, false
}

func validClock(year, month, day, hour, minute int) bool {
	return year >= 1970 && month >= 1 && month <= 12 && day >= 1 && day <= 31 && hour >= 0 && hour <= 23 && minute >= 0 && minute <= 59
}

func buildEventICS(message domain.Message, when time.Time) string {
	end := when.Add(time.Hour)
	stamp := func(t time.Time) string { return t.UTC().Format("20060102T150405Z") }
	summary := strings.ReplaceAll(message.Subject, "\r", " ")
	summary = strings.ReplaceAll(summary, "\n", " ")
	if summary == "" {
		summary = "邮件日程"
	}
	description := message.Sender + "\n" + truncateRunes(firstNonEmpty(message.BodyText, message.Snippet), 500)
	return strings.Join([]string{
		"BEGIN:VCALENDAR",
		"VERSION:2.0",
		"PRODID:-//NexusMail//ZH",
		"CALSCALE:GREGORIAN",
		"METHOD:PUBLISH",
		"BEGIN:VEVENT",
		fmt.Sprintf("UID:nexusmail-%d@local", message.ID),
		fmt.Sprintf("DTSTAMP:%s", stamp(time.Now())),
		fmt.Sprintf("DTSTART:%s", stamp(when)),
		fmt.Sprintf("DTEND:%s", stamp(end)),
		"SUMMARY:" + escapeICSValue(summary),
		"DESCRIPTION:" + escapeICSValue(description),
		"END:VEVENT",
		"END:VCALENDAR",
		"",
	}, "\r\n")
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func truncateRunes(value string, max int) string {
	runes := []rune(value)
	if len(runes) <= max {
		return value
	}
	return string(runes[:max])
}

func escapeICSValue(value string) string {
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, ";", "\\;")
	value = strings.ReplaceAll(value, ",", "\\,")
	value = strings.ReplaceAll(value, "\r\n", "\\n")
	value = strings.ReplaceAll(value, "\n", "\\n")
	return value
}
