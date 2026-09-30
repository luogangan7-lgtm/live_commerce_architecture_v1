// directory.go: GetStoreList (the chain store directory), its in-process cache and the connect probe
// (§7.2, §7.3, F10, F18).
//
// The list is the chain's store list, not merchant data, so the cache is keyed by CvsType alone (the
// environment is fixed per Client): at most three entries exist and more stores never thrash it. It
// stays fresh until the next 20:00 Asia/Taipei (ECPay refreshes daily then, F10); at most one fetch
// per key per 10 minutes; concurrent misses coalesce; a failed fetch is remembered for the rest of
// its 10 minutes so an HTTP 403 (30 min ban, F11) is not hammered.
// ponytail: per-process cache; move to a PG table if multi-instance cold fetch latency matters.

package ecpay

import (
	"context"
	"encoding/json"
	"io"
	"net/url"
	"strings"
	"time"
)

const failureMemory = 10 * time.Minute

// dirEntry is the cache state of one CvsType. stores may be stale after expires (CachedStore reports
// that as cacheFresh=false); inflight is non-nil while one goroutine fetches.
type dirEntry struct {
	stores      map[string]Store
	expires     time.Time
	failedUntil time.Time
	inflight    chan struct{}
}

var taipei = func() *time.Location {
	if l, err := time.LoadLocation("Asia/Taipei"); err == nil {
		return l
	}
	return time.FixedZone("Asia/Taipei", 8*3600) // no tzdata dependency (E8); Taiwan has no DST
}()

// nextRefresh is the first 20:00 Asia/Taipei strictly after t.
func nextRefresh(t time.Time) time.Time {
	l := t.In(taipei)
	n := time.Date(l.Year(), l.Month(), l.Day(), 20, 0, 0, 0, taipei)
	if !n.After(l) {
		n = n.AddDate(0, 0, 1)
	}
	return n
}

func validCvsType(t string) bool { return t == "UNIMART" || t == "FAMI" || t == "HILIFE" }

func storeListForm(cr Credentials, cvsType string) url.Values {
	v := url.Values{"MerchantID": {cr.MerchantID}, "CvsType": {cvsType}}
	v.Set(macField, CheckMac(v, cr.HashKey, cr.HashIV))
	return v
}

// Probe is the connect probe (§7.3): GetStoreList UNIMART within 10 s; RtnCode=1 is success, any
// other outcome is ErrUnavailable. It is read-only and allowed for LIVE credentials. It stops reading
// as soon as RtnCode is known, so it does not download the whole list.
func (c *Client) Probe(ctx context.Context, cr Credentials) error {
	if !merchantIDRE.MatchString(cr.MerchantID) || cr.HashKey == "" || cr.HashIV == "" {
		return ErrInvalid
	}
	resp, cancel, err := c.post(ctx, callTimeout, pathStoreList, storeListForm(cr, "UNIMART"))
	if err != nil {
		return ErrUnavailable
	}
	defer cancel()
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return ErrUnavailable
	}
	rtn, _, err := decodeStoreList(io.LimitReader(resp.Body, maxStoreListBody), true)
	if err != nil || rtn != "1" {
		return ErrUnavailable
	}
	return nil
}

// StoreDirectory returns the chain directory keyed by exact StoreId (§7.2), fetching with the caller's
// keys on a miss. A cached failure or any failed fetch is ErrUnavailable; an OKMARTC2C-style chain has
// no CvsType and never reaches here (see CVSType). The returned map is shared: callers must not
// mutate it.
func (c *Client) StoreDirectory(ctx context.Context, cvsType string, cr Credentials) (map[string]Store, error) {
	if !validCvsType(cvsType) {
		return nil, ErrInvalid
	}
	if !merchantIDRE.MatchString(cr.MerchantID) || cr.HashKey == "" || cr.HashIV == "" {
		return nil, ErrInvalid
	}
	for {
		c.mu.Lock()
		e := c.dirs[cvsType]
		if e == nil {
			e = &dirEntry{}
			c.dirs[cvsType] = e
		}
		now := c.now()
		switch {
		case e.stores != nil && now.Before(e.expires):
			s := e.stores
			c.mu.Unlock()
			return s, nil
		case now.Before(e.failedUntil):
			c.mu.Unlock()
			return nil, ErrUnavailable
		case e.inflight != nil:
			wait := e.inflight
			c.mu.Unlock()
			select {
			case <-wait: // the leader finished; re-evaluate (success, remembered failure, or take over)
			case <-ctx.Done():
				return nil, ErrUnavailable
			}
			continue
		}
		e.inflight = make(chan struct{})
		done := e.inflight
		c.mu.Unlock()

		stores, err := c.fetchDirectory(ctx, cvsType, cr)

		c.mu.Lock()
		e.inflight = nil
		fin := c.now()
		switch {
		case err == nil:
			e.stores, e.expires, e.failedUntil = stores, nextRefresh(fin), time.Time{}
		case ctx.Err() == nil:
			// A real failure is remembered so a 403 ban (F11) is not hammered. A caller-side
			// cancellation is not: it says nothing about ECPay, so the next caller may fetch.
			e.failedUntil = fin.Add(failureMemory)
		}
		c.mu.Unlock()
		close(done)
		if err != nil {
			return nil, ErrUnavailable
		}
		return stores, nil
	}
}

func (c *Client) fetchDirectory(ctx context.Context, cvsType string, cr Credentials) (map[string]Store, error) {
	resp, cancel, err := c.post(ctx, storeListTimeout, pathStoreList, storeListForm(cr, cvsType))
	if err != nil {
		return nil, ErrUnavailable
	}
	defer cancel()
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, ErrUnavailable
	}
	rtn, stores, err := decodeStoreList(io.LimitReader(resp.Body, maxStoreListBody+1), false)
	// The +1 lets an over-cap list fail as a truncated JSON document, never as a short directory.
	if err != nil || rtn != "1" || len(stores) == 0 {
		return nil, ErrUnavailable
	}
	return stores, nil
}

// CachedStore looks a store up in the cache only, never fetching (the inline map-return path, C10).
// hit says the store id is in the cached directory; cacheFresh says the directory is within its
// 20:00 window, so a stale hit must not be trusted as verification.
func (c *Client) CachedStore(cvsType, storeID string) (Store, bool, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.dirs[cvsType]
	if e == nil || e.stores == nil {
		return Store{}, false, false
	}
	s, hit := e.stores[storeID]
	return s, hit, c.now().Before(e.expires)
}

// decodeStoreList streams the GetStoreList JSON. The response layout (nesting of the list under the
// chain) is documented only as rows of {StoreId, StoreName, StoreAddr, StorePhone} (F10, F18), so any
// object carrying a StoreId at any depth is a row and the top-level RtnCode decides success
// (TCV07/TCV12 record the real layout). stopAtRtn returns as soon as RtnCode has been read.
func decodeStoreList(r io.Reader, stopAtRtn bool) (rtn string, stores map[string]Store, err error) {
	dec := json.NewDecoder(r)
	dec.UseNumber()
	stores = map[string]Store{}
	tok, err := dec.Token()
	if d, ok := tok.(json.Delim); err != nil || !ok || d != '{' {
		return "", nil, ErrUnavailable
	}
	var walk func(depth int, top bool) error
	scalar := func(t json.Token) (string, bool) {
		switch v := t.(type) {
		case string:
			return v, true
		case json.Number:
			return v.String(), true
		}
		return "", false
	}
	var walkArray func(depth int) error
	walkArray = func(depth int) error {
		for dec.More() {
			t, err := dec.Token()
			if err != nil {
				return err
			}
			if d, ok := t.(json.Delim); ok {
				if depth > 12 {
					return ErrUnavailable
				}
				if d == '{' {
					err = walk(depth+1, false)
				} else {
					err = walkArray(depth + 1)
				}
				if err != nil {
					return err
				}
			}
		}
		_, err := dec.Token() // ']'
		return err
	}
	walk = func(depth int, top bool) error {
		var st Store
		var hasID bool
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return err
			}
			key, _ := kt.(string)
			vt, err := dec.Token()
			if err != nil {
				return err
			}
			if d, ok := vt.(json.Delim); ok {
				if depth > 12 {
					return ErrUnavailable
				}
				if d == '{' {
					err = walk(depth+1, false)
				} else {
					err = walkArray(depth + 1)
				}
				if err != nil {
					return err
				}
				continue
			}
			s, isScalar := scalar(vt)
			if !isScalar {
				continue
			}
			switch {
			case top && key == "RtnCode":
				rtn = s
				if stopAtRtn {
					return io.EOF
				}
			case key == "StoreId":
				st.ID, hasID = strings.TrimRight(s, " "), true
			case key == "StoreName":
				st.Name = strings.TrimRight(s, " ")
			case key == "StoreAddr":
				st.Address = strings.TrimRight(s, " ")
			case key == "StorePhone":
				st.Phone = strings.TrimRight(s, " ")
			}
		}
		if _, err := dec.Token(); err != nil { // '}'
			return err
		}
		if hasID && st.ID != "" {
			stores[st.ID] = st
		}
		return nil
	}
	if err := walk(1, true); err != nil {
		if err == io.EOF && stopAtRtn {
			return rtn, stores, nil
		}
		return "", nil, ErrUnavailable
	}
	return rtn, stores, nil
}
