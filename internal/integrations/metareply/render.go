package metareply

import (
	"regexp"
	"strings"

	"livecommerce/internal/claims"
	"livecommerce/internal/command"
)

// Reply text `claim-link/v1` (meta-claims-intake-v1 §6.3): one fixed sentence per locale plus the
// buyer claim URL. No merchant text, price, discount or delivery promise. The wording is the
// engineering default of the brief; the owner may replace it (O6) by changing this table only.
var replySentence = map[string]string{
	"zh-TW": "感謝留言！點此確認你的喊單：",
	"zh-CN": "感谢留言！点此确认你的下单：",
	"en":    "Thanks for your comment! Confirm your claim here:",
}

// originPattern is the control.storefront_domains.origin CHECK (0020): https + lowercase DNS name.
var originPattern = regexp.MustCompile(`^https://([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)

// RenderClaimLink returns sentence + " " + origin + "/" + locale + "/claim#t=" + token. origin is
// the ACTIVE storefront domain as stored (`https://host`); a bare host is accepted and prefixed. The
// token stays in the URL fragment so it never reaches a server log. Invalid locale, origin or
// token is command.ErrInvalid. Pure.
func RenderClaimLink(locale, origin string, token claims.LinkToken) (string, error) {
	sentence, ok := replySentence[locale]
	if !ok {
		return "", command.ErrInvalid
	}
	if !strings.HasPrefix(origin, "https://") {
		origin = "https://" + origin
	}
	if len(origin) < 11 || len(origin) > 261 || !originPattern.MatchString(origin) {
		return "", command.ErrInvalid
	}
	if _, err := claims.ParseLinkToken(string(token)); err != nil {
		return "", command.ErrInvalid
	}
	return sentence + " " + origin + "/" + locale + "/claim#t=" + string(token), nil
}
