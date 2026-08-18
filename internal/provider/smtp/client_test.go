package smtp

import (
	"errors"
	"testing"

	gosmtp "github.com/emersion/go-smtp"
)

func TestClassifySMTPFailure(t *testing.T) {
	temporary := classify("data", &gosmtp.SMTPError{Code: 451, Message: "try later"}, true)
	var delivery *DeliveryError
	if !errors.As(temporary, &delivery) || !delivery.Temporary || delivery.Unknown || delivery.Code != 451 {
		t.Fatalf("451 classification: %#v", delivery)
	}
	permanent := classify("rcpt", &gosmtp.SMTPError{Code: 550, Message: "no such user"}, false)
	if !errors.As(permanent, &delivery) || delivery.Temporary || delivery.Unknown || delivery.Code != 550 {
		t.Fatalf("550 classification: %#v", delivery)
	}
	ambiguous := classify("data-commit", errors.New("connection reset"), true)
	if !errors.As(ambiguous, &delivery) || !delivery.Unknown || delivery.Temporary {
		t.Fatalf("ambiguous classification: %#v", delivery)
	}
}
