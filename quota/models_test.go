package main

import (
	"encoding/json"
	"testing"
)

func TestEntireCatalogue(t *testing.T) {
	raw, err := handleModels("model.for_auth", []byte(`{"AuthProvider":"entire"}`))
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		Result struct {
			Provider string
			Models   []struct{ ID string }
		}
	}
	if e := json.Unmarshal(raw, &env); e != nil {
		t.Fatal(e)
	}
	if len(env.Result.Models) != 13 || env.Result.Provider != "entire" {
		t.Fatal("incomplete catalogue")
	}
	seen := map[string]bool{}
	for _, m := range env.Result.Models {
		if seen[m.ID] || m.ID == "" {
			t.Fatal("duplicate or empty model")
		}
		seen[m.ID] = true
	}
	auth := buildAuthData(authStorage{}, hostSummary{}, "Entire")
	if auth.Prefix != "entire" {
		t.Fatal("missing namespace")
	}
}
