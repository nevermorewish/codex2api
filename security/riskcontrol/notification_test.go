package riskcontrol

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestRiskControlNotificationSTARTTLS(t *testing.T) {
	// httptest supplies a local test certificate; trust it only for this call.
	certificateServer := httptest.NewTLSServer(nil)
	certificate := certificateServer.TLS.Certificates[0]
	root := certificateServer.Certificate()
	certificateServer.Close()
	roots := x509.NewCertPool()
	roots.AddCert(root)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	message := make(chan string, 1)
	serverErrors := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverErrors <- err
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		_, _ = fmt.Fprint(conn, "220 localhost ESMTP\r\n")
		reader := bufio.NewReader(conn)
		encrypted := false
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				serverErrors <- err
				return
			}
			switch {
			case strings.HasPrefix(line, "EHLO"):
				_, _ = fmt.Fprint(conn, "250-localhost\r\n250 STARTTLS\r\n")
			case strings.HasPrefix(line, "STARTTLS"):
				_, _ = fmt.Fprint(conn, "220 Ready for TLS\r\n")
				secured := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12})
				if err := secured.Handshake(); err != nil {
					serverErrors <- err
					return
				}
				conn, reader, encrypted = secured, bufio.NewReader(secured), true
			case strings.HasPrefix(line, "MAIL FROM:"), strings.HasPrefix(line, "RCPT TO:"):
				if !encrypted {
					serverErrors <- fmt.Errorf("envelope sent before TLS")
					return
				}
				_, _ = fmt.Fprint(conn, "250 OK\r\n")
			case strings.HasPrefix(line, "DATA"):
				_, _ = fmt.Fprint(conn, "354 End with dot\r\n")
				var body strings.Builder
				for {
					line, err := reader.ReadString('\n')
					if err != nil {
						serverErrors <- err
						return
					}
					if line == ".\r\n" {
						break
					}
					body.WriteString(line)
				}
				_, _ = fmt.Fprint(conn, "250 Accepted\r\n")
				message <- body.String()
				return
			default:
				serverErrors <- fmt.Errorf("unexpected SMTP command: %s", line)
				return
			}
		}
	}()
	c := DefaultConfig()
	c.SMTPHost, c.EmailFrom, c.EmailTo = "127.0.0.1", "from@example.test", "to@example.test"
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	c.SMTPPort, _ = strconv.Atoi(port)
	event := Event{ID: "test-event", APIKeyID: 12, Excerpt: "private prompt must not appear", ViolationCount: 2, AutoBanned: true, Decision: Decision{Action: "keyword_block"}}
	err = sendNoticeTLS(context.Background(), c, event, &tls.Config{RootCAs: roots, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case body := <-message:
		for _, value := range []string{"API Key ID: 12", "Violations: 2", "Banned: true", "Event: test-event"} {
			if !strings.Contains(body, value) {
				t.Fatalf("missing notification metadata %q", value)
			}
		}
		if strings.Contains(body, event.Excerpt) {
			t.Fatal("prompt leaked into notification")
		}
	case err := <-serverErrors:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("SMTP server did not receive message")
	}
}
