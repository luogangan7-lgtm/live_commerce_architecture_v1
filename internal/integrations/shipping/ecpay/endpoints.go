// endpoints.go: the fixed ECPay hosts and paths per environment. There is no caller-supplied host.
//
// External constants (retrieved 2026-09-29 UTC, https://developers.ecpay.com.tw/):
//   - map        POST {host}/Express/map                            /8795/ (F1)
//   - create     POST {host}/Express/Create                         /8809/ (F5)
//   - query      POST {host}/Helper/QueryLogisticsTradeInfo/V5      /7418/ (F10)
//   - store list POST {host}/Helper/GetStoreList                    /47496/ (F10, F18)
//   - print      {host}/Express/PrintUniMartC2COrderInfo (/7406/), /Express/PrintFAMIC2COrderInfo
//     (/8848/), /Express/PrintHILIFEC2COrderInfo (/8858/), B2C /helper/printTradeDocument (/8875/) (F8)
//
// Hosts: stage logistics-stage.ecpay.com.tw, production logistics.ecpay.com.tw (F1, F11).

package ecpay

const (
	hostSandbox = "https://logistics-stage.ecpay.com.tw"
	hostLive    = "https://logistics.ecpay.com.tw"

	pathMap       = "/Express/map"
	pathCreate    = "/Express/Create"
	pathQuery     = "/Helper/QueryLogisticsTradeInfo/V5"
	pathStoreList = "/Helper/GetStoreList"
)

var printPaths = map[string]string{
	"UNIMARTC2C": "/Express/PrintUniMartC2COrderInfo",
	"FAMIC2C":    "/Express/PrintFAMIC2COrderInfo",
	"HILIFEC2C":  "/Express/PrintHILIFEC2COrderInfo",
	"UNIMART":    "/helper/printTradeDocument",
	"FAMI":       "/helper/printTradeDocument",
	"HILIFE":     "/helper/printTradeDocument",
}

func hostFor(env Environment) (string, bool) {
	switch env {
	case EnvSandbox:
		return hostSandbox, true
	case EnvLive:
		return hostLive, true
	}
	return "", false
}
