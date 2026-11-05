package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOpenRouterEmbedder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		json.NewDecoder(r.Body).Decode(&in)
		if in["dimensions"] != float64(EmbedDims) || len(in["input"].([]any)) != 2 || r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("bad request %v", in)
		}
		// Out of order on purpose: vectors are placed by index.
		json.NewEncoder(w).Encode(map[string]any{"data": []any{
			map[string]any{"index": 1, "embedding": []float32{0, 1}},
			map[string]any{"index": 0, "embedding": []float32{1, 0}},
		}, "usage": map[string]any{"cost": 0.000001}})
	}))
	defer srv.Close()
	e := NewOpenRouterEmbedder("k", "openai/text-embedding-3-small").(*orEmbedder)
	e.url = srv.URL
	v, err := e.Embed(context.Background(), []string{"a", "b"})
	if err != nil || v[0][0] != 1 || v[1][1] != 1 || e.Cost() != 0.000001 {
		t.Fatalf("%v %v cost %v", v, err, e.Cost())
	}
}
