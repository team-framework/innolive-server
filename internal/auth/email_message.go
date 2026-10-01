package auth

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"html"
	"mime"
	"strings"
	"time"
)

// EmailPurpose는 인증 코드 메일의 용도다(#388). 용도마다 제목과 안내 문구가 다르다.
type EmailPurpose string

const (
	EmailPurposeSignup        EmailPurpose = "signup"
	EmailPurposeAccountSetup  EmailPurpose = "account_setup"
	EmailPurposePasswordReset EmailPurpose = "password_reset"
)

type emailCopy struct {
	subject string
	title   string
	intro   string
	ignore  string
}

func verificationEmailCopy(purpose EmailPurpose) emailCopy {
	switch purpose {
	case EmailPurposeAccountSetup:
		return emailCopy{
			subject: "[InnoLive] 계정 설정 인증 코드",
			title:   "계정 설정을 마무리해 주세요",
			intro:   "아래 코드를 입력하면 기존 계정에 이메일과 비밀번호가 연결돼요.",
			ignore:  "직접 요청하지 않았다면 이 메일은 무시해 주세요. 계정에는 아무 변화가 없어요.",
		}
	case EmailPurposePasswordReset:
		return emailCopy{
			subject: "[InnoLive] 비밀번호 변경 인증 코드",
			title:   "비밀번호를 변경하시나요?",
			intro:   "아래 코드를 입력하면 비밀번호가 바뀌고, 다른 기기에서는 로그아웃돼요.",
			ignore:  "직접 요청하지 않았다면 이 메일은 무시해 주세요. 비밀번호는 그대로예요.",
		}
	default:
		return emailCopy{
			subject: "[InnoLive] 회원가입 인증 코드",
			title:   "InnoLive에 오신 걸 환영해요",
			intro:   "아래 코드를 입력하면 가입이 완료돼요.",
			ignore:  "직접 요청하지 않았다면 이 메일은 무시해 주세요.",
		}
	}
}

// buildVerificationEmail은 텍스트와 HTML을 함께 담은 multipart/alternative 메일을
// 만든다. HTML을 못 그리는 메일 앱은 텍스트를 보여 준다.
func buildVerificationEmail(from, to, code string, purpose EmailPurpose, validFor time.Duration) ([]byte, error) {
	content := verificationEmailCopy(purpose)
	minutes := int(validFor.Round(time.Minute) / time.Minute)
	validity := fmt.Sprintf("이 코드는 %d분 동안 유효해요.", minutes)

	text := strings.Join([]string{
		content.title,
		"",
		content.intro,
		"",
		"인증 코드: " + code,
		"",
		validity,
		content.ignore,
		"",
		"InnoLive",
	}, "\r\n")

	// innolive.studio 랜딩과 같은 톤: 연회색 바탕, 검정 굵은 헤드라인, 어두운 박스 로고.
	// 메일 앱은 SVG·웹폰트를 대부분 막으므로 로고는 글자와 표로 그린다.
	htmlBody := fmt.Sprintf(`<!doctype html>
<html lang="ko"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><meta name="color-scheme" content="light"></head>
<body style="margin:0;padding:0;background:#f8f8f8;">
<table role="presentation" width="100%%" cellpadding="0" cellspacing="0" style="background:#f8f8f8;">
<tr><td align="center" style="padding:40px 20px;">
<table role="presentation" width="100%%" cellpadding="0" cellspacing="0" style="max-width:440px;font-family:'Wanted Sans','Pretendard',-apple-system,BlinkMacSystemFont,'Apple SD Gothic Neo','Malgun Gothic',sans-serif;color:#000000;word-break:keep-all;">
<tr><td style="padding:0 4px 28px;">
<table role="presentation" cellpadding="0" cellspacing="0"><tr>
<td style="font-size:26px;font-weight:300;letter-spacing:-0.5px;color:#313131;line-height:32px;">Inno</td>
<td style="padding-left:2px;"><span style="display:inline-block;background:#313131;border-radius:3px;padding:0 7px;font-size:26px;font-weight:500;letter-spacing:-0.5px;color:#ffffff;line-height:32px;">Live</span></td>
</tr></table>
</td></tr>
<tr><td style="background:#ffffff;border-radius:24px;padding:36px 28px;">
<p style="margin:0;font-size:26px;font-weight:700;line-height:1.3;letter-spacing:-0.5px;color:#000000;">%s</p>
<p style="margin:14px 0 0;font-size:16px;line-height:1.6;color:#313131;">%s</p>
<p style="margin:32px 0 0;padding:18px 0 18px 8px;background:#000000;border-radius:999px;text-align:center;font-size:30px;font-weight:700;letter-spacing:8px;color:#ffffff;">%s</p>
<p style="margin:14px 0 0;text-align:center;font-size:14px;color:#6b6b6b;">%s</p>
</td></tr>
<tr><td style="padding:24px 8px 0;font-size:13px;line-height:1.6;color:#8a8a8a;">%s</td></tr>
<tr><td style="padding:16px 8px 0;font-size:12px;color:#a0a0a0;"><a href="https://innolive.studio" style="color:#a0a0a0;text-decoration:none;">innolive.studio</a></td></tr>
</table>
</td></tr>
</table>
</body></html>`, html.EscapeString(content.title), html.EscapeString(content.intro), html.EscapeString(code), html.EscapeString(validity), html.EscapeString(content.ignore))

	boundaryBytes := make([]byte, 12)
	if _, err := rand.Read(boundaryBytes); err != nil {
		return nil, err
	}
	boundary := "innolive-" + hex.EncodeToString(boundaryBytes)

	var message bytes.Buffer
	message.WriteString("To: " + to + "\r\n")
	message.WriteString("From: " + from + "\r\n")
	message.WriteString("Subject: " + mime.BEncoding.Encode("UTF-8", content.subject) + "\r\n")
	message.WriteString("MIME-Version: 1.0\r\n")
	message.WriteString("Content-Type: multipart/alternative; boundary=\"" + boundary + "\"\r\n\r\n")
	writeMIMEPart(&message, boundary, "text/plain; charset=UTF-8", text)
	writeMIMEPart(&message, boundary, "text/html; charset=UTF-8", htmlBody)
	message.WriteString("--" + boundary + "--\r\n")
	return message.Bytes(), nil
}

// writeMIMEPart는 본문을 base64로 76자마다 줄을 나눠 쓴다(SMTP 줄 길이 제한).
func writeMIMEPart(message *bytes.Buffer, boundary, contentType, body string) {
	message.WriteString("--" + boundary + "\r\n")
	message.WriteString("Content-Type: " + contentType + "\r\n")
	message.WriteString("Content-Transfer-Encoding: base64\r\n\r\n")
	encoded := base64.StdEncoding.EncodeToString([]byte(body))
	for len(encoded) > 76 {
		message.WriteString(encoded[:76] + "\r\n")
		encoded = encoded[76:]
	}
	message.WriteString(encoded + "\r\n")
}
