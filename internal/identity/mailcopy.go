package identity

// Mail content for the emailed code and the "account already exists" notice (contract §3): subject
// without the code, text plus minimal HTML with the code, purpose, "valid 10 minutes" and what to do
// if the reader did not ask. No links, no images, no tracking pixels: a link would put a secret in a
// URL (I11, rejected magic links) and remote content would leak that the mail was opened.

import (
	"html"
	"strings"

	"livecommerce/internal/mail"
)

// copyText is one locale x purpose text set. Intro is followed by the code; Ignore closes the mail.
type copyText struct{ Subject, Intro, Valid, Ignore string }

var codeCopy = map[string]map[string]copyText{
	"en": {
		"signup": {"Confirm your email address", "Use this code to finish creating your account:", "The code is valid for 10 minutes.", "If you did not request this, ignore this email; no account is created without the code."},
		"login":  {"Your sign-in code", "Use this code to finish signing in:", "The code is valid for 10 minutes.", "If this was not you, ignore this email and reset your password."},
		"reset":  {"Your password reset code", "Use this code to reset your password:", "The code is valid for 10 minutes.", "If you did not request this, ignore this email; your password is unchanged."},
	},
	"zh-CN": {
		"signup": {"确认您的邮箱地址", "请使用以下验证码完成账号注册：", "验证码 10 分钟内有效。", "如果这不是您本人的操作，请忽略此邮件；没有验证码就不会创建账号。"},
		"login":  {"您的登录验证码", "请使用以下验证码完成登录：", "验证码 10 分钟内有效。", "如果这不是您本人的操作，请忽略此邮件并重置密码。"},
		"reset":  {"您的密码重置验证码", "请使用以下验证码重置密码：", "验证码 10 分钟内有效。", "如果这不是您本人的操作，请忽略此邮件；您的密码不会改变。"},
	},
	"zh-TW": {
		"signup": {"確認您的電子郵件地址", "請使用以下驗證碼完成帳號註冊：", "驗證碼 10 分鐘內有效。", "如果這不是您本人的操作，請忽略此郵件；沒有驗證碼就不會建立帳號。"},
		"login":  {"您的登入驗證碼", "請使用以下驗證碼完成登入：", "驗證碼 10 分鐘內有效。", "如果這不是您本人的操作，請忽略此郵件並重設密碼。"},
		"reset":  {"您的密碼重設驗證碼", "請使用以下驗證碼重設密碼：", "驗證碼 10 分鐘內有效。", "如果這不是您本人的操作，請忽略此郵件；您的密碼不會改變。"},
	},
}

var existsCopy = map[string]copyText{
	"en":    {"Your account already exists", "Someone tried to sign up with this email address, but an account already exists for it.", "Sign in with your password, or reset the password if you forgot it.", "If this was not you, ignore this email."},
	"zh-CN": {"您的账号已存在", "有人尝试使用此邮箱地址注册，但该邮箱已有账号。", "请使用密码登录；如果忘记密码，请重置密码。", "如果这不是您本人的操作，请忽略此邮件。"},
	"zh-TW": {"您的帳號已存在", "有人嘗試使用此電子郵件地址註冊，但該地址已有帳號。", "請使用密碼登入；如果忘記密碼，請重設密碼。", "如果這不是您本人的操作，請忽略此郵件。"},
}

func validLocale(l string) bool { _, ok := codeCopy[l]; return ok }

// codeMail builds the code mail for purpose (signup|login|reset). code is six ASCII digits.
func codeMail(to, locale, purpose, code string) mail.Message {
	c := codeCopy[locale][purpose]
	text := strings.Join([]string{c.Intro, "", code, "", c.Valid, c.Ignore, ""}, "\n")
	page := "<p>" + html.EscapeString(c.Intro) + "</p>" +
		`<p style="font-size:24px;letter-spacing:4px"><strong>` + html.EscapeString(code) + "</strong></p>" +
		"<p>" + html.EscapeString(c.Valid) + "</p><p>" + html.EscapeString(c.Ignore) + "</p>"
	return mail.Message{To: to, Subject: c.Subject, Text: text, HTML: page}
}

// existsMail builds the sign-up "account already exists" notice (no code, PD6).
func existsMail(to, locale string) mail.Message {
	c := existsCopy[locale]
	text := strings.Join([]string{c.Intro, c.Valid, c.Ignore, ""}, "\n")
	page := "<p>" + html.EscapeString(c.Intro) + "</p><p>" + html.EscapeString(c.Valid) + "</p><p>" + html.EscapeString(c.Ignore) + "</p>"
	return mail.Message{To: to, Subject: c.Subject, Text: text, HTML: page}
}
