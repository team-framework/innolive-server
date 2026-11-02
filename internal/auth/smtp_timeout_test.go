package auth

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"
)

// SMTP 서버가 연결만 받고 응답하지 않으면 발송이 기한 없이 멈춘다. 가입·재설정
// 요청이 그대로 붙잡히고 연결이 쌓인다(P1-16).
func TestSMTPSendHonorsDeadlineWhenServerStalls(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			// 인사말을 보내지 않고 붙잡는다.
			t.Cleanup(func() { _ = connection.Close() })
		}
	}()
	host, port, _ := net.SplitHostPort(listener.Addr().String())
	portNumber, _ := strconv.Atoi(port)
	sender, err := NewSMTPVerificationEmailSender(EmailAuthConfig{SMTPHost: host, SMTPPort: portNumber, SenderAddress: "no-reply@example.com", CodeTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- sender.SendVerificationCode(ctx, "member@example.com", "123456", EmailPurposeSignup) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("send succeeded against a stalled SMTP server")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SMTP send ignored the deadline")
	}
}
