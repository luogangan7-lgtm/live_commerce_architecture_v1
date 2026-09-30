// Package meta holds the black-box Meta webhook protocol tests for internal/integrations/meta (files
// *_test.go, package meta_test). It owns no production code.
//
// It never contacts Meta or a database: signatures, payload shapes and replay cases are synthetic
// (MOCK), so a pass here says nothing about live Meta delivery (LIVE probes are MCI11).
package meta
