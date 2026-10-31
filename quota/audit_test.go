package main

import (
	"encoding/json"
	"testing"
)

func TestParsePreservesOperatorFields(t *testing.T) {
	resetPluginState(t)
	original := map[string]any{"type": "entire", "entire_token": "synthetic-audit-token", "disabled": true, "prefix": "custom", "proxy_url": "http://localhost:7890", "label": "Account A", "priority": 9, "custom_field": "keep"}
	doc, _ := json.Marshal(original)
	raw, _ := json.Marshal(authParseRequest{RawJSON: doc, FileName: "account-a.json"})
	out, err := handleAuthParse(raw)
	if err != nil {
		t.Fatal(err)
	}
	var env struct{ Result authParseResponse }
	if e := json.Unmarshal(out, &env); e != nil {
		t.Fatal(e)
	}
	a := env.Result.Auth
	if a == nil || !a.Disabled || a.Prefix != "custom" || a.ProxyURL != "http://localhost:7890" || a.FileName != "account-a.json" {
		t.Fatal("operator fields lost")
	}
	var saved map[string]any
	json.Unmarshal(a.StorageJSON, &saved)
	if saved["custom_field"] != "keep" || saved["priority"] != float64(9) {
		t.Fatal("storage fields lost")
	}
}
