// Package tokenopen owns the OPEN half of BISU token custody (meta-ads-v1 A-4, ads-graph G1): the
// HPKE private keys and Keyring.Open, which turns a sealed ads token back into plaintext for one
// dispatcher call.
//
// It must only be imported by cmd/ads-worker. cmd/api holds public keys only and must never be able
// to read a token back: `go list -deps ./cmd/api` must not list this package (gate MA11, and
// ./cmd/api's merchant_ads_test.go). It never touches PG (the lease-fenced loader
// integration.load_meta_ads_token supplies the ciphertext), never logs or returns key material,
// never caches a plaintext token, and never seals.
//
// External: none (a private-keys file read at startup, path from the environment; never a network call).
package tokenopen
