package main

import (
	"encoding/json"
	"testing"
)

func TestEntireRouteFailsLocally(t *testing.T) {
	for _, model := range []string{"entire/claude-opus-5-5", "entire/gpt-6.1-sol"} {
		req, _ := json.Marshal(map[string]any{"RequestedModel": model, "Model": "claude-opus-5-5", "Stream": true})
		raw, err := interceptEntire(req)
		if err != nil {
			t.Fatal(err)
		}
		var out struct {
			Result struct {
				Terminate    bool
				StatusCode   int
				ResponseBody []byte
			}
		}
		if e := json.Unmarshal(raw, &out); e != nil {
			t.Fatal(e)
		}
		if !out.Result.Terminate || out.Result.StatusCode != 501 {
			t.Fatal("Entire route was not stopped")
		}
	}
}
func TestOtherRoutesAreUnchanged(t *testing.T) {
	body := []byte(`{"model":"wb/model","messages":[]}`)
	req, _ := json.Marshal(map[string]any{"Model": "wb/model", "Body": body})
	raw, err := interceptEntire(req)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Result struct {
			Terminate bool
			Body      []byte
		}
	}
	json.Unmarshal(raw, &out)
	if out.Result.Terminate || string(out.Result.Body) != string(body) {
		t.Fatal("other provider changed")
	}
}
