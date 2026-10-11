package mail

import (
	"bufio"
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/gochathub/gochathub-server/internal/service"
)

// fakeSMTP speaks just enough SMTP to accept one message; returns the DATA.
func fakeSMTP(t *testing.T) (port string, got chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	got = make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		r := bufio.NewReader(c)
		say := func(s string) { c.Write([]byte(s + "\r\n")) }
		say("220 fake")
		var data strings.Builder
		inData := false
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			switch {
			case inData && line == ".\r\n":
				inData = false
				got <- data.String()
				say("250 ok")
			case inData:
				data.WriteString(line)
			case strings.HasPrefix(line, "EHLO"):
				say("250 fake")
			case strings.HasPrefix(line, "DATA"):
				inData = true
				say("354 go")
			case strings.HasPrefix(line, "QUIT"):
				say("221 bye")
				return
			default:
				say("250 ok")
			}
		}
	}()
	_, port, _ = net.SplitHostPort(ln.Addr().String())
	return port, got
}

func TestSMTPSend(t *testing.T) {
	port, got := fakeSMTP(t)
	s := &SMTP{Host: "127.0.0.1", Port: port, From: "chat@x.test", TLS: "none"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Send(ctx, service.Mail{To: "a@x.test", Subject: "hi", Body: "line1\nline2\n"}); err != nil {
		t.Fatal(err)
	}
	msg := <-got
	for _, want := range []string{"To: a@x.test\r\n", "Subject: hi\r\n", "line1\r\nline2\r\n"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message missing %q:\n%s", want, msg)
		}
	}
}

func TestSMTPRejectsHeaderInjection(t *testing.T) {
	s := &SMTP{Host: "127.0.0.1", Port: "1", From: "chat@x.test", TLS: "none"}
	if err := s.Send(context.Background(), service.Mail{To: "a@x.test\r\nBcc: evil@x.test", Subject: "s"}); err == nil {
		t.Fatal("want header-injection error")
	}
}

func TestSMTPRefusesPlaintextAuth(t *testing.T) {
	port, _ := fakeSMTP(t)
	s := &SMTP{Host: "127.0.0.1", Port: port, From: "chat@x.test", User: "u", Pass: "p", TLS: "none"}
	if err := s.Send(context.Background(), service.Mail{To: "a@x.test", Subject: "s"}); err == nil || !strings.Contains(err.Error(), "plaintext") {
		t.Fatalf("want plaintext-auth refusal, got %v", err)
	}
}
