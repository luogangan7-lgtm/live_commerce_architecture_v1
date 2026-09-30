// kv.go: the lenient k=v parser used for ECPay bodies (Create/Query responses and status forms) and
// small text sanitizers. ECPay does not document whether response values are percent-encoded, so each
// value is decoded when it is valid percent-encoding and kept raw otherwise; the MAC then decides.

package ecpay

import (
	"net/url"
	"strings"
)

// parseKV splits "k=v&k=v", rejecting an empty key or a duplicate key (ErrInvalid: a duplicate could
// make the MAC cover one value while the caller reads the other).
func parseKV(s string) (url.Values, error) {
	out := url.Values{}
	if s == "" {
		return nil, ErrInvalid
	}
	for _, pair := range strings.Split(s, "&") {
		if pair == "" {
			continue
		}
		k, v, _ := strings.Cut(pair, "=")
		if dk, err := url.QueryUnescape(k); err == nil {
			k = dk
		}
		if dv, err := url.QueryUnescape(v); err == nil {
			v = dv
		}
		if k == "" {
			return nil, ErrInvalid
		}
		if _, dup := out[k]; dup {
			return nil, ErrInvalid
		}
		out[k] = []string{v}
	}
	if len(out) == 0 {
		return nil, ErrInvalid
	}
	return out, nil
}

// clean makes a provider string safe to hand to SQL: valid UTF-8, no NUL, at most max bytes on a
// rune boundary. Values are never logged.
func clean(s string, max int) string {
	s = strings.ToValidUTF8(strings.ReplaceAll(s, "\x00", ""), "")
	if len(s) > max {
		s = strings.ToValidUTF8(s[:max], "")
	}
	return s
}
