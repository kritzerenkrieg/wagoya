package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Embedder turns texts into vectors for hybrid fact search (J2 c).
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	Model() string
}

// EmbedDims is the vector size requested (text-embedding-3 models can
// shorten their vectors; 512 keeps 10k facts at about 20 MB).
const EmbedDims = 512

const openRouterEmbedURL = "https://openrouter.ai/api/v1/embeddings"

type orEmbedder struct {
	key, model, url string
	http            *http.Client
	costMu          sync.Mutex
	cost            float64
}

// NewOpenRouterEmbedder embeds with model through OpenRouter's embeddings API.
func NewOpenRouterEmbedder(key, model string) Embedder {
	return &orEmbedder{key: key, model: model, url: openRouterEmbedURL, http: &http.Client{Timeout: 30 * time.Second}}
}

func (e *orEmbedder) Model() string { return e.model }

// Cost is what the embeddings cost so far (J6).
func (e *orEmbedder) Cost() float64 {
	e.costMu.Lock()
	defer e.costMu.Unlock()
	return e.cost
}

func (e *orEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	body := map[string]any{"model": e.model, "input": texts}
	if strings.Contains(e.model, "text-embedding-3") {
		body["dimensions"] = EmbedDims
	}
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.url, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+e.key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Title", "wagoya")
	res, err := e.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embeddings: %w", err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("embeddings: %w", err)
	}
	if res.StatusCode/100 != 2 {
		s := strings.TrimSpace(string(raw))
		if len(s) > 300 {
			s = s[:300]
		}
		return nil, fmt.Errorf("embeddings: HTTP %d: %s", res.StatusCode, s)
	}
	var out struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
		Usage struct {
			Cost float64 `json:"cost"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("embeddings: bad response: %w", err)
	}
	if len(out.Data) != len(texts) {
		return nil, fmt.Errorf("embeddings: got %d vectors for %d texts", len(out.Data), len(texts))
	}
	vecs := make([][]float32, len(texts))
	for _, d := range out.Data {
		if d.Index < 0 || d.Index >= len(texts) || len(d.Embedding) == 0 {
			return nil, fmt.Errorf("embeddings: bad vector index %d", d.Index)
		}
		vecs[d.Index] = d.Embedding
	}
	e.costMu.Lock()
	e.cost += out.Usage.Cost
	e.costMu.Unlock()
	return vecs, nil
}
