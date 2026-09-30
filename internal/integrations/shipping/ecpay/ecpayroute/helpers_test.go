// helpers_test.go: formatting helper for the redaction assertions of route_test.go.
package ecpayroute

import "fmt"

func fmtAny(format string, v any) string { return fmt.Sprintf(format, v) }
