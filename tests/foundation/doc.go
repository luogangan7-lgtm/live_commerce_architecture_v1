// Package foundation holds the real-PostgreSQL foundation and browser acceptance tests (files
// *_test.go, package foundation_test; browser gates need -tags browser). It owns no production code.
//
// It never runs against a developer or production database: every gate provisions a disposable
// container through scripts/dev/test-local.sh or test-focused.sh and refuses to start without their
// LC_TEST_DATABASE_ALLOWED guard. See docs/delivery/GATES.md for the mode-to-gate map.
package foundation
