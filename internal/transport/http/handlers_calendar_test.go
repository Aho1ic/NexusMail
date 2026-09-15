package http

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"nexusmail/internal/domain"
)

func TestMessageCalendarServesInlineICS(t *testing.T) {
	h := newHarness(t)
	fixture := h.seedFeed(1)
	id := fixture.ids[0]
	path := fmt.Sprintf("/api/v1/messages/%d/calendar", id)
	response := h.do(http.MethodGet, path, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("GET calendar = %d: %s", response.Code, response.Body.String())
	}
	if ct := response.Header().Get("Content-Type"); !strings.Contains(ct, "text/calendar") {
		t.Fatalf("Content-Type = %q, want text/calendar", ct)
	}
	if cd := response.Header().Get("Content-Disposition"); !strings.Contains(cd, "inline") {
		t.Fatalf("Content-Disposition = %q, want inline", cd)
	}
	payload := response.Body.String()
	for _, needle := range []string{"BEGIN:VCALENDAR", "BEGIN:VEVENT", "END:VEVENT", "END:VCALENDAR"} {
		if !strings.Contains(payload, needle) {
			t.Fatalf("ICS missing %q:\n%s", needle, payload)
		}
	}
}

func TestBuildEventICSEscapesAndTruncates(t *testing.T) {
	message := domain.Message{ID: 9, Subject: "a;b,c", BodyText: strings.Repeat("字", 800), Sender: "S <s@x.com>"}
	ics := buildEventICS(message, time.UnixMilli(1_700_000_000_000))
	if !strings.Contains(ics, `SUMMARY:a\;b\,c`) {
		t.Fatalf("summary not escaped:\n%s", ics)
	}
	if strings.Contains(ics, strings.Repeat("字", 801)) {
		t.Fatal("description was not truncated")
	}
}

func TestParseCalendarTime(t *testing.T) {
	when, ok := parseCalendarTime("会议定在 2026年3月12日 14:30 开始")
	if !ok || when.Year() != 2026 || when.Month() != time.March || when.Day() != 12 || when.Hour() != 14 || when.Minute() != 30 {
		t.Fatalf("parseCalendarTime = %v %v", when, ok)
	}
	if _, ok := parseCalendarTime("没有日期的一封信"); ok {
		t.Fatal("parsed a date from text without one")
	}
}
