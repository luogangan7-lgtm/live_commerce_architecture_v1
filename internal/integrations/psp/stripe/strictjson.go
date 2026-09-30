// strictjson.go: strict JSON decoding for Stripe API responses and webhook payloads.
// It never interprets values; it only rejects ambiguous JSON before projection.
//
// Ownership: integration_worker (contracts/stripe-psp-v1.md §5.6, §5.8).
// Why not encoding/json.Unmarshal: it silently accepts duplicate keys (last wins) and
// replaces invalid UTF-8, which would let a crafted body carry two different answers.
// Dependencies: encoding/json's tokenizer only. Callers: parseSession, parseList,
// parseAccount and WebhookVerifier.Verify in this package.

package stripe

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"unicode/utf8"
)

const maxJSONDepth = 64

var errStrictJSON = errors.New("stripe: strict json")

// decodeStrict parses raw into map[string]any / []any / json.Number / string / bool / nil.
// It rejects invalid UTF-8, duplicate object keys at any depth, depth > 64 and any
// non-whitespace trailing data.
func decodeStrict(raw []byte) (any, error) {
	if !utf8.Valid(raw) {
		return nil, errStrictJSON
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	v, err := decodeValue(dec, 0)
	if err != nil {
		return nil, errStrictJSON
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errStrictJSON
	}
	return v, nil
}

func decodeValue(dec *json.Decoder, depth int) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return tok, nil
	}
	if depth+1 > maxJSONDepth {
		return nil, errStrictJSON
	}
	switch delim {
	case '{':
		obj := map[string]any{}
		for dec.More() {
			keyTok, err := dec.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyTok.(string)
			if !ok {
				return nil, errStrictJSON
			}
			if _, dup := obj[key]; dup {
				return nil, errStrictJSON
			}
			val, err := decodeValue(dec, depth+1)
			if err != nil {
				return nil, err
			}
			obj[key] = val
		}
		if _, err := dec.Token(); err != nil { // closing '}'
			return nil, err
		}
		return obj, nil
	case '[':
		arr := []any{}
		for dec.More() {
			val, err := decodeValue(dec, depth+1)
			if err != nil {
				return nil, err
			}
			arr = append(arr, val)
		}
		if _, err := dec.Token(); err != nil { // closing ']'
			return nil, err
		}
		return arr, nil
	}
	return nil, errStrictJSON
}

// jsonObject is a typed accessor over a decoded object. Every getter distinguishes
// "absent or null" (ok=true, present=false) from "wrong type" (ok=false).
type jsonObject map[string]any

func (o jsonObject) str(key string) (value string, present, ok bool) {
	raw, exists := o[key]
	if !exists || raw == nil {
		return "", false, true
	}
	s, isStr := raw.(string)
	return s, isStr, isStr
}

func (o jsonObject) boolean(key string) (value, present, ok bool) {
	raw, exists := o[key]
	if !exists || raw == nil {
		return false, false, true
	}
	b, isBool := raw.(bool)
	return b, isBool, isBool
}

// integer accepts only a JSON integer that fits int64; fractions and exponents fail.
func (o jsonObject) integer(key string) (value *int64, ok bool) {
	raw, exists := o[key]
	if !exists || raw == nil {
		return nil, true
	}
	n, isNum := raw.(json.Number)
	if !isNum {
		return nil, false
	}
	i, err := n.Int64()
	if err != nil {
		return nil, false
	}
	return &i, true
}

func (o jsonObject) object(key string) (value jsonObject, present, ok bool) {
	raw, exists := o[key]
	if !exists || raw == nil {
		return nil, false, true
	}
	m, isObj := raw.(map[string]any)
	return jsonObject(m), isObj, isObj
}
