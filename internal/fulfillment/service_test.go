package fulfillment

import "testing"

const testID = "11111111-1111-4111-8111-111111111111"

func validInput() ServiceInput {
	return ServiceInput{
		MarketID: testID, Country: "TW", Code: "tw_home", PolicyVersion: 1,
		NameHans: "宅配", NameHant: "宅配", NameEN: "Home delivery",
		DeliveryKind: "home", Mode: "MANUAL", Enabled: true, Visible: true,
	}
}

func TestValidServiceInput(t *testing.T) {
	t.Parallel()

	if !validServiceInput(validInput()) {
		t.Fatal("valid input was rejected")
	}

	tests := []struct {
		name string
		edit func(*ServiceInput)
	}{
		{"market", func(in *ServiceInput) { in.MarketID = "bad" }},
		{"country", func(in *ServiceInput) { in.Country = "tw" }},
		{"code", func(in *ServiceInput) { in.Code = "Bad" }},
		{"negative expected version", func(in *ServiceInput) { in.ExpectedVersion = -1 }},
		{"missing policy", func(in *ServiceInput) { in.PolicyVersion = 0 }},
		{"blank label", func(in *ServiceInput) { in.NameHans = " \t" }},
		{"invalid utf8", func(in *ServiceInput) { in.NameHant = string([]byte{0xff}) }},
		{"control label", func(in *ServiceInput) { in.NameEN = "home\n" }},
		{"long label", func(in *ServiceInput) { in.NameEN = string(make([]byte, 121)) }},
		{"kind", func(in *ServiceInput) { in.DeliveryKind = "locker" }},
		{"foreign cvs", func(in *ServiceInput) { in.Country = "US"; in.DeliveryKind = "cvs_711" }},
		{"mode", func(in *ServiceInput) { in.Mode = "AUTO" }},
		{"manual binding", func(in *ServiceInput) { in.BindingID = testID; in.BindingVersion = 1 }},
		{"partial binding id", func(in *ServiceInput) { in.Mode = "API"; in.BindingID = testID }},
		{"partial binding version", func(in *ServiceInput) { in.Mode = "API"; in.BindingVersion = 1 }},
		{"enabled api", func(in *ServiceInput) { in.Mode = "API" }},
		{"sort low", func(in *ServiceInput) { in.SortOrder = -1 }},
		{"sort high", func(in *ServiceInput) { in.SortOrder = 1001 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := validInput()
			tt.edit(&in)
			if validServiceInput(in) {
				t.Fatalf("invalid input accepted: %+v", in)
			}
		})
	}

	draft := validInput()
	draft.Mode = "API"
	draft.Enabled = false
	draft.BindingID = testID
	draft.BindingVersion = 2
	if !validServiceInput(draft) {
		t.Fatal("valid disabled API draft was rejected")
	}

	// taiwan-cvs-logistics-v1 R-2: an ENABLED API service is valid only with a binding and a CVS kind (SQL then requires the
	// binding to be the store's qualified, enabled ecpay_logistics profile); all four chains are service kinds.
	for _, kind := range []string{"cvs_711", "cvs_familymart", "cvs_hilife", "cvs_okmart"} {
		api := validInput()
		api.DeliveryKind, api.Mode, api.BindingID, api.BindingVersion = kind, "API", testID, 1
		if !validServiceInput(api) {
			t.Fatalf("enabled API service of kind %s with a binding was rejected", kind)
		}
		manual := validInput()
		manual.DeliveryKind = kind
		if !validServiceInput(manual) {
			t.Fatalf("manual service of kind %s was rejected", kind)
		}
	}
	homeAPI := validInput()
	homeAPI.Mode, homeAPI.BindingID, homeAPI.BindingVersion = "API", testID, 1
	if validServiceInput(homeAPI) {
		t.Fatal("enabled API home delivery accepted: only CVS kinds have an adapter")
	}
}

func TestStopOnlyTransition(t *testing.T) {
	t.Parallel()

	in := validInput()
	in.ExpectedVersion = 3
	in.PolicyVersion = 7
	old := Service{
		MarketID: in.MarketID, Country: in.Country, Code: in.Code, Version: 3,
		PolicyMethod: "delivery:" + in.Code, PolicyVersion: 7, Currency: "TWD",
		NameHans: in.NameHans, NameHant: in.NameHant, NameEN: in.NameEN,
		DeliveryKind: in.DeliveryKind, Mode: in.Mode, Enabled: true, Visible: true,
		SortOrder: in.SortOrder,
	}
	in.Enabled = false
	if !isStopOnly(old, in) {
		t.Fatal("exact enabled-to-disabled transition was rejected")
	}
	in.Visible = false
	if !isStopOnly(old, in) {
		t.Fatal("stop with visible true-to-false was rejected")
	}

	mutations := []func(*ServiceInput){
		func(v *ServiceInput) { v.PolicyVersion++ },
		func(v *ServiceInput) { v.NameEN = "Changed" },
		func(v *ServiceInput) { v.SortOrder++ },
		func(v *ServiceInput) { v.ExpectedVersion++ },
	}
	for i, mutate := range mutations {
		changed := in
		mutate(&changed)
		if isStopOnly(old, changed) {
			t.Fatalf("mutation %d was accepted as stop-only", i)
		}
	}

	old.Visible = false
	in.Visible = true
	if isStopOnly(old, in) {
		t.Fatal("visibility false-to-true was accepted as stop-only")
	}
}
