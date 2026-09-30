package main

import "testing"

func TestNewMerchantRefundJobsIsInsertOnly(t *testing.T) {
	if _, err := newMerchantRefundJobs(nil); err == nil {
		t.Fatal("a nil pool must be refused")
	}
}
