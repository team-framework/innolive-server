package auth

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"testing"
	"time"
)

func newPasswordResetTestHandler(t *testing.T) (http.Handler, *memoryEmailAccountStore, *recordingVerificationEmailSender) {
	t.Helper()
	pending := newMemoryPendingEmailSignupStore()
	accounts := &memoryEmailAccountStore{}
	sender := &recordingVerificationEmailSender{}
	tokens := testTokenService(newMemoryRefreshStore())
	service := newTestEmailAuthService(t, pending, accounts, sender, tokens)
	config, _ := NewTokenHTTPConfig(false, nil)
	handler := MountAuthHTTPWithServices(http.NotFoundHandler(), tokens, nil, nil, service, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), config)

	start := serveEmailJSON(t, handler, http.MethodPost, "/auth/v2/native/sign-up", map[string]string{"email": "member@example.com", "password": "old password 123", "name": "회원"}, nil)
	var started struct {
		SignupToken string `json:"signup_token"`
	}
	_ = json.Unmarshal(start.Body.Bytes(), &started)
	if verify := serveEmailJSON(t, handler, http.MethodPost, "/auth/native/verify-email", map[string]string{"signup_token": started.SignupToken, "verification_code": sender.code}, nil); verify.Code != http.StatusOK {
		t.Fatalf("signup verify = %d %s", verify.Code, verify.Body.String())
	}
	*sender = recordingVerificationEmailSender{}
	return handler, accounts, sender
}

func startPasswordReset(t *testing.T, handler http.Handler, email string) string {
	t.Helper()
	response := serveEmailJSON(t, handler, http.MethodPost, "/auth/password/reset", map[string]string{"email": email, "new_password": "new password 456"}, nil)
	var body struct {
		Status     string `json:"status"`
		ResetToken string `json:"reset_token"`
	}
	_ = json.Unmarshal(response.Body.Bytes(), &body)
	if response.Code != http.StatusOK || body.Status != "verification_email_sent" || body.ResetToken == "" {
		t.Fatalf("password reset start = %d %s", response.Code, response.Body.String())
	}
	return body.ResetToken
}

func TestPasswordResetChangesPasswordAfterEmailCode(t *testing.T) {
	handler, accounts, sender := newPasswordResetTestHandler(t)
	token := startPasswordReset(t, handler, "Member@Example.com")
	if sender.code == "" || sender.purpose != EmailPurposePasswordReset || sender.recipient != "member@example.com" {
		t.Fatalf("reset mail = %+v", sender)
	}

	wrong := serveEmailJSON(t, handler, http.MethodPost, "/auth/password/reset/verify", map[string]string{"reset_token": token, "verification_code": "000000"}, nil)
	if wrong.Code != http.StatusBadRequest || accounts.resets != 0 {
		t.Fatalf("wrong code = %d %s, resets %d", wrong.Code, wrong.Body.String(), accounts.resets)
	}
	verify := serveEmailJSON(t, handler, http.MethodPost, "/auth/password/reset/verify", map[string]string{"reset_token": token, "verification_code": sender.code}, nil)
	if verify.Code != http.StatusOK || !strings.Contains(verify.Body.String(), "access_token") {
		t.Fatalf("reset verify = %d %s", verify.Code, verify.Body.String())
	}

	old := serveEmailJSON(t, handler, http.MethodPost, "/auth/sign-in", map[string]string{"email": "member@example.com", "password": "old password 123"}, nil)
	if old.Code != http.StatusUnauthorized {
		t.Fatalf("old password sign-in = %d, want 401", old.Code)
	}
	fresh := serveEmailJSON(t, handler, http.MethodPost, "/auth/sign-in", map[string]string{"email": "member@example.com", "password": "new password 456"}, nil)
	if fresh.Code != http.StatusOK {
		t.Fatalf("new password sign-in = %d %s", fresh.Code, fresh.Body.String())
	}
}

func TestPasswordResetHidesUnknownEmail(t *testing.T) {
	handler, _, sender := newPasswordResetTestHandler(t)
	token := startPasswordReset(t, handler, "nobody@example.com")
	if sender.code != "" {
		t.Fatal("reset for an unknown email sent a verification mail")
	}
	verify := serveEmailJSON(t, handler, http.MethodPost, "/auth/password/reset/verify", map[string]string{"reset_token": token, "verification_code": "123456"}, nil)
	if verify.Code != http.StatusBadRequest {
		t.Fatalf("unknown email verify = %d, want 400", verify.Code)
	}
}

func TestPasswordResetAndSignupTokensCannotBeSwapped(t *testing.T) {
	handler, accounts, sender := newPasswordResetTestHandler(t)
	token := startPasswordReset(t, handler, "member@example.com")
	code := sender.code

	// 비밀번호 변경 토큰을 가입 확인에 쓰면 거절되고 코드는 소비되지 않는다.
	crossed := serveEmailJSON(t, handler, http.MethodPost, "/auth/native/verify-email", map[string]string{"signup_token": token, "verification_code": code}, nil)
	if crossed.Code == http.StatusOK {
		t.Fatalf("reset token accepted by signup verify: %s", crossed.Body.String())
	}
	verify := serveEmailJSON(t, handler, http.MethodPost, "/auth/password/reset/verify", map[string]string{"reset_token": token, "verification_code": code}, nil)
	if verify.Code != http.StatusOK || accounts.resets != 1 {
		t.Fatalf("reset after crossed attempt = %d %s", verify.Code, verify.Body.String())
	}

	signup := serveEmailJSON(t, handler, http.MethodPost, "/auth/v2/native/sign-up", map[string]string{"email": "other@example.com", "password": "other password 1", "name": "다른 회원"}, nil)
	var started struct {
		SignupToken string `json:"signup_token"`
	}
	_ = json.Unmarshal(signup.Body.Bytes(), &started)
	swapped := serveEmailJSON(t, handler, http.MethodPost, "/auth/password/reset/verify", map[string]string{"reset_token": started.SignupToken, "verification_code": sender.code}, nil)
	if swapped.Code != http.StatusBadRequest {
		t.Fatalf("signup token accepted by reset verify = %d", swapped.Code)
	}
}

func TestVerificationEmailHasPurposeSubjectAndHTML(t *testing.T) {
	subjects := map[string]bool{}
	for _, purpose := range []EmailPurpose{EmailPurposeSignup, EmailPurposeAccountSetup, EmailPurposePasswordReset} {
		message, err := buildVerificationEmail("InnoLive <no-reply@example.com>", "member@example.com", "482913", purpose, 5*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		text := string(message)
		if !strings.Contains(text, "multipart/alternative") || !strings.Contains(text, "text/html; charset=UTF-8") || !strings.Contains(text, "text/plain; charset=UTF-8") {
			t.Fatalf("%s mail is not multipart text+html", purpose)
		}
		var subject string
		for _, line := range strings.Split(text, "\r\n") {
			if strings.HasPrefix(line, "Subject: ") {
				subject, err = new(mime.WordDecoder).DecodeHeader(strings.TrimPrefix(line, "Subject: "))
				if err != nil {
					t.Fatal(err)
				}
			}
			if len(line) > 998 {
				t.Fatalf("%s mail line exceeds SMTP limit", purpose)
			}
		}
		if !strings.HasPrefix(subject, "[InnoLive] ") {
			t.Fatalf("%s subject = %q", purpose, subject)
		}
		subjects[subject] = true
		htmlPart := decodeHTMLPart(t, text)
		if !strings.Contains(htmlPart, "482913") || !strings.Contains(htmlPart, "5분 동안 유효") {
			t.Fatalf("%s html part missing code or validity", purpose)
		}
		// 로고는 SVG를 못 그리는 메일 앱을 위해 PNG를 인라인 첨부하고 cid로 참조한다.
		if !strings.Contains(htmlPart, `src="cid:innolive-logo"`) || !strings.Contains(text, "Content-ID: <innolive-logo>") || !strings.Contains(text, "multipart/related") {
			t.Fatalf("%s mail does not embed the logo inline", purpose)
		}
	}
	if len(subjects) != 3 {
		t.Fatalf("subjects are not purpose specific: %v", subjects)
	}
}

func decodeHTMLPart(t *testing.T, message string) string {
	t.Helper()
	marker := "Content-Type: text/html; charset=UTF-8\r\nContent-Transfer-Encoding: base64\r\n\r\n"
	index := strings.Index(message, marker)
	if index < 0 {
		t.Fatal("html part missing")
	}
	rest := message[index+len(marker):]
	encoded := strings.ReplaceAll(rest[:strings.Index(rest, "--")], "\r\n", "")
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	return string(decoded)
}
