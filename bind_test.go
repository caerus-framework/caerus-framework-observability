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
