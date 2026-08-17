// Package search wraps OpenSearch (§29).
//
// The client speaks the REST API directly rather than pulling in a vendor SDK:
// the surface used here is small, and index definitions are the part that
// actually matters — Persian and Arabic text is unusable without the right
// normalisation, which is what most of this file is about.
package search

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/sobh/messenger/backend/internal/config"
)

// Index names, suffixed onto the configured prefix.
const (
	IndexNews     = "news"
	IndexUsers    = "users"
	IndexChats    = "chats"
	IndexMessages = "messages"
)

type Client struct {
	http     *http.Client
	cfg      config.Search
	baseURL  string
	username string
	password string
}

func New(cfg config.Search) *Client {
	base := "http://localhost:9200"
	if len(cfg.Addresses) > 0 {
		base = strings.TrimRight(cfg.Addresses[0], "/")
	}
	return &Client{
		http:     &http.Client{Timeout: 15 * time.Second},
		cfg:      cfg,
		baseURL:  base,
		username: cfg.Username,
		password: cfg.Password,
	}
}

func (c *Client) Enabled() bool { return c.cfg.Enabled }

func (c *Client) indexName(name string) string {
	return c.cfg.IndexPfx + "-" + name
}

// persianAnalysis is the analyzer definition every text field uses.
//
// Persian and Arabic text needs three separate corrections before it can be
// matched reliably (§29):
//
//   - Character folding: Arabic ي/ك and Persian ی/ک are visually and
//     semantically the same letter to a user, but different code points.
//   - Zero-width non-joiner: Persian compounds like "می‌رود" contain U+200C.
//     Replacing it with a space lets both the joined and separated spellings
//     match.
//   - Diacritics and digits: harakat are optional in writing, and Persian,
//     Arabic and Latin digits must all fold to one form.
//
// Without these, a search for "کتاب" simply fails to find "كتاب".
var persianAnalysis = map[string]any{
	"analysis": map[string]any{
		"char_filter": map[string]any{
			"zero_width_spaces": map[string]any{
				"type":     "mapping",
				"mappings": []string{"\\u200C=>\\u0020", "\\u200D=>", "\\u200E=>", "\\u200F=>"},
			},
			"persian_letters": map[string]any{
				"type": "mapping",
				"mappings": []string{
					"\\u064A=>\\u06CC", // Arabic yeh   -> Persian yeh
					"\\u0649=>\\u06CC", // Alef maksura -> Persian yeh
					"\\u0643=>\\u06A9", // Arabic kaf   -> Persian kaf
					"\\u0629=>\\u0647", // Teh marbuta  -> heh
					"\\u0623=>\\u0627", // Alef with hamza above -> alef
					"\\u0625=>\\u0627", // Alef with hamza below -> alef
					"\\u0622=>\\u0627", // Alef with madda -> alef
				},
			},
		},
		"filter": map[string]any{
			"persian_stop": map[string]any{
				"type":      "stop",
				"stopwords": "_persian_",
			},
			"edge_ngram_filter": map[string]any{
				"type":     "edge_ngram",
				"min_gram": 2,
				"max_gram": 20,
			},
		},
		"analyzer": map[string]any{
			// The general-purpose analyzer for indexing and matching.
			"sobh_text": map[string]any{
				"type":        "custom",
				"char_filter": []string{"zero_width_spaces", "persian_letters"},
				"tokenizer":   "standard",
				"filter": []string{
					"lowercase",
					"decimal_digit",         // Persian/Arabic digits -> Latin
					"arabic_normalization",  // strips tatweel and normalises hamza
					"persian_normalization", // Persian-specific letter folding
					"persian_stop",
				},
			},
			// Prefix matching for as-you-type search over names and titles.
			"sobh_prefix": map[string]any{
				"type":        "custom",
				"char_filter": []string{"zero_width_spaces", "persian_letters"},
				"tokenizer":   "standard",
				"filter": []string{
					"lowercase", "decimal_digit",
					"arabic_normalization", "persian_normalization",
					"edge_ngram_filter",
				},
			},
			// The query-time counterpart: queries are not n-grammed, or every
			// query prefix would match everything.
			"sobh_prefix_search": map[string]any{
				"type":        "custom",
				"char_filter": []string{"zero_width_spaces", "persian_letters"},
				"tokenizer":   "standard",
				"filter": []string{
					"lowercase", "decimal_digit",
					"arabic_normalization", "persian_normalization",
				},
			},
		},
	},
}

// textField is a searchable field with a prefix sub-field for type-ahead.
func textField() map[string]any {
	return map[string]any{
		"type":     "text",
		"analyzer": "sobh_text",
		"fields": map[string]any{
			"prefix": map[string]any{
				"type":            "text",
				"analyzer":        "sobh_prefix",
				"search_analyzer": "sobh_prefix_search",
			},
			"keyword": map[string]any{"type": "keyword", "ignore_above": 256},
		},
	}
}

// indexDefinitions describes every index the platform maintains.
var indexDefinitions = map[string]map[string]any{
	IndexNews: {
		"properties": map[string]any{
			"id":           map[string]any{"type": "keyword"},
			"slug":         map[string]any{"type": "keyword"},
			"locale":       map[string]any{"type": "keyword"},
			"title":        textField(),
			"subtitle":     textField(),
			"lead":         textField(),
			"body":         textField(),
			"category_id":  map[string]any{"type": "keyword"},
			"tags":         textField(),
			"is_breaking":  map[string]any{"type": "boolean"},
			"published_at": map[string]any{"type": "date"},
			"view_count":   map[string]any{"type": "long"},
		},
	},
	IndexUsers: {
		"properties": map[string]any{
			"id":           map[string]any{"type": "keyword"},
			"username":     textField(),
			"display_name": textField(),
			"about":        textField(),
		},
	},
	IndexChats: {
		"properties": map[string]any{
			"id":           map[string]any{"type": "keyword"},
			"type":         map[string]any{"type": "keyword"},
			"title":        textField(),
			"description":  textField(),
			"username":     textField(),
			"member_count": map[string]any{"type": "integer"},
			"is_public":    map[string]any{"type": "boolean"},
		},
	},
	IndexMessages: {
		"properties": map[string]any{
			"id": map[string]any{"type": "keyword"},
			// Scoping is by chat, and the caller's chats are resolved from
			// PostgreSQL at query time.
			//
			// The obvious alternative — indexing each message with the list of
			// members who may read it — cannot work here. A channel can have
			// hundreds of thousands of subscribers, so every message document
			// would carry that many ids, and one person joining or leaving
			// would mean rewriting every message in the chat. Chat membership
			// is small and already indexed in PostgreSQL; a message is not the
			// place to duplicate it.
			"chat_id":    map[string]any{"type": "keyword"},
			"sender_id":  map[string]any{"type": "keyword"},
			"content":    textField(),
			"seq":        map[string]any{"type": "long"},
			"created_at": map[string]any{"type": "date"},
		},
	},
}

// EnsureIndices creates any missing index with its analyzer and mapping.
func (c *Client) EnsureIndices(ctx context.Context) error {
	if !c.Enabled() {
		return nil
	}

	for name, mappings := range indexDefinitions {
		index := c.indexName(name)

		exists, err := c.indexExists(ctx, index)
		if err != nil {
			return err
		}
		if exists {
			continue
		}

		body := map[string]any{
			"settings": mergeSettings(map[string]any{
				"number_of_shards":   1,
				"number_of_replicas": 1,
				// Persian text benefits from a larger n-gram window than the
				// default 1, which would otherwise reject the prefix filter.
				"max_ngram_diff": 20,
			}, persianAnalysis),
			"mappings": mappings,
		}
		if err := c.request(ctx, http.MethodPut, "/"+index, body, nil); err != nil {
			return fmt.Errorf("search: create index %q: %w", index, err)
		}
	}
	return nil
}

func mergeSettings(base, analysis map[string]any) map[string]any {
	merged := make(map[string]any, len(base)+len(analysis))
	for k, v := range base {
		merged[k] = v
	}
	for k, v := range analysis {
		merged[k] = v
	}
	return merged
}

func (c *Client) indexExists(ctx context.Context, index string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, c.baseURL+"/"+index, nil)
	if err != nil {
		return false, err
	}
	c.authorize(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return false, fmt.Errorf("search: check index: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	return resp.StatusCode == http.StatusOK, nil
}

// Index writes or replaces a document.
func (c *Client) Index(ctx context.Context, index, documentID string, document any) error {
	if !c.Enabled() {
		return nil
	}
	path := fmt.Sprintf("/%s/_doc/%s", c.indexName(index), documentID)
	return c.request(ctx, http.MethodPut, path, document, nil)
}

// Delete removes a document. A missing document is not an error: deletions are
// replayed from a queue and must be idempotent.
func (c *Client) Delete(ctx context.Context, index, documentID string) error {
	if !c.Enabled() {
		return nil
	}
	path := fmt.Sprintf("/%s/_doc/%s", c.indexName(index), documentID)
	err := c.request(ctx, http.MethodDelete, path, nil, nil)
	if err != nil && strings.Contains(err.Error(), "404") {
		return nil
	}
	return err
}

// Hit is one search result.
type Hit struct {
	ID        string         `json:"id"`
	Score     float64        `json:"score"`
	Source    map[string]any `json:"source"`
	Highlight map[string]any `json:"highlight,omitempty"`
}

type searchResponse struct {
	Hits struct {
		Total struct {
			Value int `json:"value"`
		} `json:"total"`
		Hits []struct {
			ID        string           `json:"_id"`
			Score     float64          `json:"_score"`
			Source    map[string]any   `json:"_source"`
			Highlight map[string][]any `json:"highlight"`
		} `json:"hits"`
	} `json:"hits"`
}

// Query runs a raw OpenSearch query body against an index.
func (c *Client) Query(ctx context.Context, index string, body map[string]any) ([]Hit, int, error) {
	if !c.Enabled() {
		return nil, 0, nil
	}

	var response searchResponse
	path := fmt.Sprintf("/%s/_search", c.indexName(index))
	if err := c.request(ctx, http.MethodPost, path, body, &response); err != nil {
		return nil, 0, err
	}

	hits := make([]Hit, 0, len(response.Hits.Hits))
	for _, raw := range response.Hits.Hits {
		hit := Hit{ID: raw.ID, Score: raw.Score, Source: raw.Source}
		if len(raw.Highlight) > 0 {
			hit.Highlight = make(map[string]any, len(raw.Highlight))
			for field, fragments := range raw.Highlight {
				hit.Highlight[field] = fragments
			}
		}
		hits = append(hits, hit)
	}
	return hits, response.Hits.Total.Value, nil
}

// Healthy reports whether the cluster is reachable.
func (c *Client) Healthy(ctx context.Context) error {
	if !c.Enabled() {
		return nil
	}
	return c.request(ctx, http.MethodGet, "/_cluster/health", nil, nil)
}

func (c *Client) request(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("search: encode request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("search: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	c.authorize(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("search: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("search: %s %s returned %d: %s",
			method, path, resp.StatusCode, bytes.TrimSpace(detail))
	}

	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("search: decode response: %w", err)
	}
	return nil
}

func (c *Client) authorize(req *http.Request) {
	if c.username != "" {
		req.SetBasicAuth(c.username, c.password)
	}
}
