package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/mail"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	sestypes "github.com/aws/aws-sdk-go-v2/service/sesv2/types"

	"github.com/gochathub/gochathub-server/internal/service"
)

// New builds the sender for a provider id from its decrypted field values.
// Field names match the registry in service.MailProviders.
func New(provider string, v map[string]string) (service.Mailer, error) {
	switch provider {
	case "smtp":
		return &SMTP{Host: v["host"], Port: v["port"], User: v["user"], Pass: v["password"], From: v["from"], TLS: v["tls"]}, nil
	case "postmark":
		return &Postmark{Token: v["server_token"], Stream: v["message_stream"], From: v["from"]}, nil
	case "sendgrid":
		return &SendGrid{Key: v["api_key"], From: v["from"]}, nil
	case "resend":
		return &Resend{Key: v["api_key"], From: v["from"]}, nil
	case "brevo":
		return &Brevo{Key: v["api_key"], From: v["from"]}, nil
	case "mailgun":
		return &Mailgun{Key: v["api_key"], Domain: v["domain"], Region: v["region"], From: v["from"]}, nil
	case "ses":
		return &SES{Region: v["region"], KeyID: v["access_key_id"], Secret: v["secret_access_key"], From: v["from"]}, nil
	}
	return nil, fmt.Errorf("unknown mail provider %q", provider)
}

// api is the shared HTTP client: bounded time, bounded error bodies.
var api = &http.Client{Timeout: 20 * time.Second}

// call performs the request and requires one of the wanted status codes. The
// error carries the provider's message (admin-visible, never the request).
func call(req *http.Request, want ...int) error {
	res, err := api.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", req.URL.Host, err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 2048))
	for _, w := range want {
		if res.StatusCode == w {
			return nil
		}
	}
	return fmt.Errorf("%s: HTTP %d: %s", req.URL.Host, res.StatusCode, strings.TrimSpace(string(body)))
}

func postJSON(ctx context.Context, endpoint string, headers map[string]string, payload any, want ...int) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return call(req, want...)
}

// always: API senders are usable once constructed (MailSettings gates the feature).
type always struct{}

func (always) Enabled(context.Context) bool { return true }

// base returns override when set (tests point senders at a fake server).
func base(override, def string) string {
	if override != "" {
		return strings.TrimRight(override, "/")
	}
	return def
}

// Postmark: POST /email with a server token; From must be a confirmed signature.
type Postmark struct {
	always
	Token, From string
	Stream      string // message stream ID; empty = Postmark's default ("outbound")
	BaseURL     string
}

func (p *Postmark) Send(ctx context.Context, m service.Mail) error {
	body := map[string]string{"From": p.From, "To": m.To, "Subject": m.Subject, "TextBody": m.Body}
	if p.Stream != "" {
		body["MessageStream"] = p.Stream
	}
	return postJSON(ctx, base(p.BaseURL, "https://api.postmarkapp.com")+"/email",
		map[string]string{"X-Postmark-Server-Token": p.Token}, body, 200)
}

// SendGrid v3 mail/send (202 Accepted).
type SendGrid struct {
	always
	Key, From string
	BaseURL   string
}

func (s *SendGrid) Send(ctx context.Context, m service.Mail) error {
	a, err := mail.ParseAddress(s.From)
	if err != nil {
		return err
	}
	from := map[string]string{"email": a.Address}
	if a.Name != "" {
		from["name"] = a.Name
	}
	return postJSON(ctx, base(s.BaseURL, "https://api.sendgrid.com")+"/v3/mail/send",
		map[string]string{"Authorization": "Bearer " + s.Key},
		map[string]any{
			"personalizations": []any{map[string]any{"to": []any{map[string]string{"email": m.To}}}},
			"from":             from,
			"subject":          m.Subject,
			"content":          []any{map[string]string{"type": "text/plain", "value": m.Body}},
		}, 202)
}

// Resend: POST /emails.
type Resend struct {
	always
	Key, From string
	BaseURL   string
}

func (r *Resend) Send(ctx context.Context, m service.Mail) error {
	return postJSON(ctx, base(r.BaseURL, "https://api.resend.com")+"/emails",
		map[string]string{"Authorization": "Bearer " + r.Key},
		map[string]any{"from": r.From, "to": []string{m.To}, "subject": m.Subject, "text": m.Body}, 200)
}

// Brevo: POST /v3/smtp/email; htmlContent is required, so the text is
// escaped into a minimal HTML body (links made clickable) beside textContent.
type Brevo struct {
	always
	Key, From string
	BaseURL   string
}

var urlRe = regexp.MustCompile(`https?://[^\s<>"]+`)

func textToHTML(s string) string {
	esc := html.EscapeString(s)
	esc = urlRe.ReplaceAllStringFunc(esc, func(u string) string { return `<a href="` + u + `">` + u + `</a>` })
	return "<html><body><p>" + strings.ReplaceAll(esc, "\n", "<br>") + "</p></body></html>"
}

func (b *Brevo) Send(ctx context.Context, m service.Mail) error {
	a, err := mail.ParseAddress(b.From)
	if err != nil {
		return err
	}
	sender := map[string]string{"email": a.Address}
	if a.Name != "" {
		sender["name"] = a.Name
	}
	return postJSON(ctx, base(b.BaseURL, "https://api.brevo.com")+"/v3/smtp/email",
		map[string]string{"api-key": b.Key},
		map[string]any{
			"sender": sender, "to": []any{map[string]string{"email": m.To}},
			"subject": m.Subject, "textContent": m.Body, "htmlContent": textToHTML(m.Body),
		}, 201)
}

// Mailgun: POST /v3/<domain>/messages, form-encoded, basic auth api:<key>.
// The region is fixed when the domain is created (us or eu).
type Mailgun struct {
	always
	Key, Domain, Region, From string
	BaseURL                   string
}

func (g *Mailgun) Send(ctx context.Context, m service.Mail) error {
	def := "https://api.mailgun.net"
	if g.Region == "eu" {
		def = "https://api.eu.mailgun.net"
	}
	form := url.Values{"from": {g.From}, "to": {m.To}, "subject": {m.Subject}, "text": {m.Body}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		base(g.BaseURL, def)+"/v3/"+url.PathEscape(g.Domain)+"/messages", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth("api", g.Key)
	return call(req, 200)
}

// SES: Amazon SES v2 SendEmail via the AWS SDK, static IAM credentials.
type SES struct {
	always
	Region, KeyID, Secret, From string
	BaseURL                     string // tests only
}

var sesRegionRe = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]$`)

func (s *SES) Send(ctx context.Context, m service.Mail) error {
	if !sesRegionRe.MatchString(s.Region) {
		return errors.New("ses: bad region")
	}
	opts := sesv2.Options{
		Region:      s.Region,
		Credentials: credentials.NewStaticCredentialsProvider(s.KeyID, s.Secret, ""),
		HTTPClient:  api,
	}
	if s.BaseURL != "" {
		opts.BaseEndpoint = aws.String(s.BaseURL)
	}
	_, err := sesv2.New(opts).SendEmail(ctx, &sesv2.SendEmailInput{
		FromEmailAddress: aws.String(s.From),
		Destination:      &sestypes.Destination{ToAddresses: []string{m.To}},
		Content: &sestypes.EmailContent{Simple: &sestypes.Message{
			Subject: &sestypes.Content{Data: aws.String(m.Subject), Charset: aws.String("UTF-8")},
			Body:    &sestypes.Body{Text: &sestypes.Content{Data: aws.String(m.Body), Charset: aws.String("UTF-8")}},
		}},
	})
	if err != nil {
		return fmt.Errorf("ses: %w", err)
	}
	return nil
}
