package cf_observability

import (
	"encoding/json"
	"testing"
)

func TestBindUnmarshalJSON(t *testing.T) {
	var one Bind
	if err := json.Unmarshal([]byte(`":9090"`), &one); err != nil {
		t.Fatal(err)
	}
	if len(one) != 1 || one[0] != ":9090" {
		t.Fatalf("string bind = %#v", one)
	}
	var many Bind
	if err := json.Unmarshal([]byte(`["127.0.0.1:9090","0.0.0.0:8080"]`), &many); err != nil {
		t.Fatal(err)
	}
	if len(many) != 2 {
		t.Fatalf("array bind = %#v", many)
	}
	var empty Bind
	if err := json.Unmarshal([]byte(`[]`), &empty); err == nil {
		t.Fatal("empty array must fail")
	}
}

func TestValidateRejectsBadBind(t *testing.T) {
	if err := validateObservabilityConfigValue(&ObservabilityConfig{Bind: Bind{"not-a-port"}}); err == nil {
		t.Fatal("expected bind validation error")
	}
	if err := validateObservabilityConfigValue(&ObservabilityConfig{}); err != nil {
		t.Fatalf("empty bind must skip parse: %v", err)
	}
	if err := validateObservabilityConfigValue(&ObservabilityConfig{Bind: Bind{":9090"}}); err != nil {
		t.Fatalf("good bind: %v", err)
	}
}
