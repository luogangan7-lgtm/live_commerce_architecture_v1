// Evidence directory (logs, scratch probes). A separate module so `go build/vet/test ./...`
// from the repo root never compiles scratch .go files left here by agents.
module livecommerce/output

go 1.27
