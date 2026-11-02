package auth

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/alicebob/miniredis/v2"
)

func newRedisLimitTestService(t *testing.T) (*EmailAuthService, *recordingVerificationEmailSender) {
	t.Helper()
	server, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Close)
	store, err := NewRedisPendingEmailSignupStore(context.Background(), EmailAuthConfig{SMTPHost: "smtp.example.com", RedisAddr: server.Addr()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	sender := &recordingVerificationEmailSender{}
	service := newTestEmailAuthService(t, store, &memoryEmailAccountStore{}, sender, testTokenService(newMemoryRefreshStore()))
	return service, sender
}

func registerLimitTestMember(t *testing.T, service *EmailAuthService, sender *recordingVerificationEmailSender) {
	t.Helper()
	token, err := service.StartSignup(context.Background(), "member@example.com", "correct horse battery staple", "203.0.113.1")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.CompleteSignup(context.Background(), token, sender.code); err != nil {
		t.Fatal(err)
	}
}

// 상한 확인과 증가가 갈라져 있으면 동시에 보낸 요청이 모두 같은 실패 수를 읽고
// 비밀번호 비교까지 간다(P0-11). 비교 전에 시도를 예약해야 상한만큼만 비교한다.
func TestConcurrentLoginsCannotExceedFailureCap(t *testing.T) {
	service, sender := newRedisLimitTestService(t)
	registerLimitTestMember(t, service, sender)

	const attempts = emailLoginMaxFailuresDefault * 3
	var wg sync.WaitGroup
	var mu sync.Mutex
	compared := 0
	for range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := service.Login(context.Background(), "member@example.com", "wrong-password", ClientInfo{})
			if errors.Is(err, ErrEmailCredentialsInvalid) {
				mu.Lock()
				compared++
				mu.Unlock()
			} else if !errors.Is(err, ErrEmailLoginThrottled) {
				t.Errorf("login error = %v", err)
			}
		}()
	}
	wg.Wait()
	if compared > emailLoginMaxFailuresDefault {
		t.Fatalf("password compared %d times, want at most %d", compared, emailLoginMaxFailuresDefault)
	}
}

// 한 IP가 여러 이메일을 돌아가며 시도하면 이메일별 상한만으로는 막히지 않는다.
func TestLoginAttemptsAreLimitedPerClientIP(t *testing.T) {
	service, sender := newRedisLimitTestService(t)
	registerLimitTestMember(t, service, sender)
	client := ClientInfo{IPAddress: "198.51.100.7", RateLimitIP: "198.51.100.7"}

	for i := range emailLoginIPMaxAttempts {
		email := fmt.Sprintf("guess-%d@example.com", i)
		if _, err := service.Login(context.Background(), email, "wrong-password", client); !errors.Is(err, ErrEmailCredentialsInvalid) {
			t.Fatalf("attempt %d = %v, want invalid credentials", i, err)
		}
	}
	if _, err := service.Login(context.Background(), "member@example.com", "correct horse battery staple", client); !errors.Is(err, ErrEmailLoginThrottled) {
		t.Fatalf("login past the IP cap = %v, want throttled", err)
	}
	if _, err := service.Login(context.Background(), "member@example.com", "correct horse battery staple", ClientInfo{IPAddress: "198.51.100.8", RateLimitIP: "198.51.100.8"}); err != nil {
		t.Fatalf("login from another IP = %v", err)
	}
}

// 재설정 토큰마다 시도 상한이 따로면, 새 토큰을 받아 가며 같은 이메일의 코드를
// 계속 추측할 수 있다(P1-22).
func TestPasswordResetCodeAttemptsAccumulatePerEmail(t *testing.T) {
	service, sender := newRedisLimitTestService(t)
	registerLimitTestMember(t, service, sender)

	guessed := 0
	for guessed < emailCodeEmailMaxAttempts {
		token, err := service.StartPasswordReset(context.Background(), "member@example.com", "another horse battery staple", fmt.Sprintf("203.0.113.%d", guessed+10))
		if err != nil {
			t.Fatalf("start reset: %v", err)
		}
		// 재발송 간격 제한을 지나 다음 토큰을 받게 한다.
		_ = service.limiter.ClearKey(context.Background(), signupResendKey(string(EmailPurposePasswordReset)+":member@example.com"))
		_ = service.limiter.ClearKey(context.Background(), signupEmailRateKey(string(EmailPurposePasswordReset)+":member@example.com"))
		wrong := "000000"
		if wrong == sender.code {
			wrong = "000001"
		}
		for range emailCodeMaxAttemptsDefault - 1 {
			if _, err := service.CompletePasswordReset(context.Background(), token, wrong, ClientInfo{}); !errors.Is(err, ErrEmailVerificationInvalid) {
				t.Fatalf("wrong reset code = %v", err)
			}
			guessed++
		}
	}

	token, err := service.StartPasswordReset(context.Background(), "member@example.com", "another horse battery staple", "203.0.113.200")
	if err != nil {
		t.Fatalf("start reset: %v", err)
	}
	if _, err := service.CompletePasswordReset(context.Background(), token, sender.code, ClientInfo{}); !errors.Is(err, ErrEmailVerificationInvalid) {
		t.Fatalf("correct code after %d guesses on the email = %v, want invalid", guessed, err)
	}
}
