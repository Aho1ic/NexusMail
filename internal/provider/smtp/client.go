package smtp

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"nexusmail/internal/domain"

	"github.com/emersion/go-sasl"
	gosmtp "github.com/emersion/go-smtp"
)

type Credential struct {
	Password    string
	AccessToken string
}

type DeliveryError struct {
	Stage     string
	Code      int
	Temporary bool
	Unknown   bool
	Err       error
}

func (e *DeliveryError) Error() string { return fmt.Sprintf("SMTP %s: %v", e.Stage, e.Err) }
func (e *DeliveryError) Unwrap() error { return e.Err }

type Client struct{ timeout time.Duration }

func New(timeout time.Duration) *Client { return &Client{timeout: timeout} }

func (c *Client) Send(ctx context.Context, account domain.Account, credential Credential, envelopeFrom string, recipients []string, messageSize int64, message io.Reader) error {
	address := net.JoinHostPort(account.SMTPHost, fmt.Sprintf("%d", account.SMTPPort))
	dialer := &net.Dialer{Timeout: c.timeout}
	var conn net.Conn
	var err error
	tlsConfig := &tls.Config{ServerName: account.SMTPHost, MinVersion: tls.VersionTLS12}
	if account.SMTPTLSMode == "implicit" {
		conn, err = tls.DialWithDialer(dialer, "tcp", address, tlsConfig)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", address)
	}
	if err != nil {
		return classify("connect", err, false)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(c.timeout))
	var client *gosmtp.Client
	if account.SMTPTLSMode == "starttls" {
		client, err = gosmtp.NewClientStartTLS(conn, tlsConfig)
		if err != nil {
			return classify("starttls", err, false)
		}
	} else {
		client = gosmtp.NewClient(conn)
	}
	defer client.Close()
	var auth sasl.Client
	if account.AuthType == "oauth2" {
		auth = &xoauth2Client{username: account.Username, token: credential.AccessToken}
	} else {
		auth = sasl.NewLoginClient(account.Username, credential.Password)
	}
	if err := client.Auth(auth); err != nil {
		return classify("auth", err, false)
	}
	var mailOptions *gosmtp.MailOptions
	if maximum, ok := client.MaxMessageSize(); ok {
		if messageSize > int64(maximum) {
			return &DeliveryError{Stage: "size", Code: 552, Err: fmt.Errorf("message size %d exceeds server limit %d", messageSize, maximum)}
		}
		mailOptions = &gosmtp.MailOptions{Size: messageSize}
	}
	if err := client.Mail(envelopeFrom, mailOptions); err != nil {
		return classify("mail-from", err, false)
	}
	for _, recipient := range recipients {
		if err := client.Rcpt(recipient, nil); err != nil {
			return classify("rcpt-to", err, false)
		}
	}
	dataWriter, err := client.Data()
	if err != nil {
		return classify("data", err, false)
	}
	if _, err := io.Copy(dataWriter, message); err != nil {
		_ = dataWriter.Close()
		return &DeliveryError{Stage: "data-write", Temporary: true, Unknown: true, Err: err}
	}
	if err := dataWriter.Close(); err != nil {
		return classify("data-commit", err, true)
	}
	_ = client.Quit()
	return nil
}

func classify(stage string, err error, ambiguous bool) error {
	result := &DeliveryError{Stage: stage, Err: err, Unknown: ambiguous}
	var smtpErr *gosmtp.SMTPError
	if errors.As(err, &smtpErr) {
		result.Code = smtpErr.Code
		result.Temporary = smtpErr.Code >= 400 && smtpErr.Code < 500
		result.Unknown = false
	} else {
		result.Temporary = !ambiguous
	}
	return result
}

type xoauth2Client struct {
	username   string
	token      string
	challenged bool
}

func (c *xoauth2Client) Start() (string, []byte, error) {
	if c.username == "" || c.token == "" {
		return "", nil, errors.New("XOAUTH2 credentials are empty")
	}
	response := fmt.Sprintf("user=%s\x01auth=Bearer %s\x01\x01", c.username, c.token)
	return "XOAUTH2", []byte(response), nil
}

func (c *xoauth2Client) Next(challenge []byte) ([]byte, error) {
	if c.challenged {
		return nil, errors.New("unexpected repeated XOAUTH2 challenge")
	}
	c.challenged = true
	if len(challenge) > 0 {
		if decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(challenge))); err == nil {
			challenge = decoded
		}
	}
	return []byte{}, nil
}
