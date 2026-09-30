package payments

import "testing"

func TestStripeSignalRejectsMalformedArgs(t *testing.T) {
	valid := paymentSignalArgs{OperationID: stripeTestAttempt, SignalID: stripeTestOrder, Version: 1}
	if !validStripeSignalArgs(valid) {
		t.Fatal("valid args rejected")
	}
	for _, args := range []paymentSignalArgs{
		{OperationID: valid.OperationID, SignalID: valid.SignalID, Version: 2},
		{OperationID: "", SignalID: valid.SignalID, Version: 1},
		{OperationID: valid.OperationID, SignalID: "", Version: 1},
		{OperationID: valid.OperationID, SignalID: "../../escape", Version: 1},
	} {
		if validStripeSignalArgs(args) {
			t.Fatalf("invalid args accepted: %+v", args)
		}
	}
}
