package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOpenRouterReportedCostAccounting(t *testing.T) {
	tests := []struct {
		name       string
		cost       any
		withTokens bool
		stream     bool
		client     string
		openRouter bool
		wantCost   int64
		wantKnown  int64
		wantMode   string
	}{
		{"non-stream", 0.25, true, false, "chat", true, 250000000, 1, "reported_cost"},
		{"stream-final-usage", 0.125, true, true, "chat", true, 125000000, 1, "reported_cost"},
		{"cross-protocol", 0.5, true, false, "messages", true, 500000000, 1, "reported_cost"},
		{"zero", 0.0, true, false, "chat", true, 0, 1, "reported_cost"},
		{"known-cost-without-tokens", 0.75, false, false, "chat", true, 750000000, 1, "reported_cost"},
		{"missing", nil, true, false, "chat", true, 20000, 1, "reported_tokens"},
		{"negative", -1.0, true, false, "chat", true, 20000, 1, "reported_tokens"},
		{"invalid", "free", true, false, "chat", true, 20000, 1, "reported_tokens"},
		{"overflow", 1e20, true, false, "chat", true, 20000, 1, "reported_tokens"},
		{"non-openrouter", 0.25, true, false, "chat", false, 20000, 1, "reported_tokens"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				usage := Object{}
				if tc.withTokens {
					usage["prompt_tokens"], usage["completion_tokens"] = 10, 5
				}
				if tc.cost != nil {
					usage["cost"] = tc.cost
				}
				if tc.stream {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\n")
					fmt.Fprintf(w, "data: %s\n\n", raw(Object{"choices": []any{}, "usage": usage}))
					fmt.Fprint(w, "data: [DONE]\n\n")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(Object{
					"id": "upstream", "choices": []any{Object{"index": 0, "message": Object{"role": "assistant", "content": "ok"}, "finish_reason": "stop"}},
					"usage": usage,
				})
			}))
			defer up.Close()

			h := newHarness(t)
			m := modelFixture("m", "chat")
			m.PricingSet, m.InputPrice, m.OutputPrice = true, 1, 2
			baseURL := up.URL
			if tc.openRouter {
				baseURL += "/openrouter.ai/api/v1"
			}
			h.configure(t, baseURL, m)
			body := requestFixture(tc.client, "m")
			body["stream"] = tc.stream
			w := h.generate(t, tc.client, body)
			requireStatus(t, w, http.StatusOK)
			if tc.stream {
				if _, err := io.ReadAll(w.Result().Body); err != nil {
					t.Fatal(err)
				}
			}
			rows, err := h.s.DB.Query("SELECT cost_nano,cost_known,usage_mode,input_tokens,output_tokens FROM requests")
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 || rows[0].Int("cost_nano") != tc.wantCost || rows[0].Int("cost_known") != tc.wantKnown || rows[0].String("usage_mode") != tc.wantMode {
				t.Fatalf("accounting=%+v want cost=%d known=%d mode=%s", rows, tc.wantCost, tc.wantKnown, tc.wantMode)
			}
		})
	}
}

func TestReportedCostIsNotRepriced(t *testing.T) {
	h := newHarness(t)
	m := modelFixture("m", "chat")
	m.PricingSet, m.InputPrice, m.OutputPrice = true, 1, 2
	s := selection{Model: m, Provider: Provider{ID: "p_test", BaseURL: "https://openrouter.ai/api/v1"}}
	id, err := h.a.Engine.admit(s, Principal{ID: "k"}, "req", "m", "chat", "", client{})
	if err != nil {
		t.Fatal(err)
	}
	h.a.Engine.finish(s, id, "", "k", Usage{Input: 10, Output: 5, Known: true, ReportedCostNano: 900000000, ReportedCostKnown: true}, 200, nil, now())
	if _, err = h.a.reprice(Object{}); err != nil {
		t.Fatal(err)
	}
	rows, err := h.s.DB.Query("SELECT cost_nano,usage_mode FROM requests WHERE id=?", id)
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Int("cost_nano") != 900000000 || rows[0].String("usage_mode") != "reported_cost" {
		t.Fatalf("reported cost was overwritten: %+v", rows)
	}
}
