// Package ecpay owns the wire side of taiwan-cvs-logistics-v1 §7 against ECPay 物流整合 API v1: the
// CheckMacValue (MAC) scheme, the e-map form and its unsigned return, the GetStoreList directory
// cache and connect probe, Express/Create, Query/V5, the print forms, the status-notification parser,
// the logistics-credential keyring (AES-256-GCM, R-1) and the CVS_ECPAY_* configuration.
//
// It never touches SQL, River or the dispatcher (package ecpayroute is the only SQL in this unit),
// never retries a Create (an UNKNOWN result is returned to the caller, which may only query; I06),
// never logs or returns keys, bodies, recipient data, trade numbers or payment codes, and never
// follows redirects. It is stdlib only: MD5 is the vendor's MAC and is never reused for our own
// storage.
//
// External hosts (TLS >= 1.2, https 443, FQDN only, F11): logistics-stage.ecpay.com.tw (SANDBOX) and
// logistics.ecpay.com.tw (LIVE) for map, GetStoreList, Create, Query and the print forms; the status
// notifications arrive from postgate(-stage).ecpay.com.tw at our own hooks origin and are only parsed
// here. Docs, retrieved 2026-09-29 unless noted: https://developers.ecpay.com.tw/8795/ (map),
// /8809/ (Create, re-fetched 2026-09-30 for IsCollection/CollectionAmount), /7418/ (Query V5),
// /47496/ (GetStoreList), /7420/ (status), /7424/ (CheckMacValue), /7406/ /8848/ /8858/ /8875/ (print).
package ecpay
