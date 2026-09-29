package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDecodeRequestBodyRejectsTrailingJSONValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
		dst  any
	}{
		{
			name: "review",
			body: `{"skill":"review-panel","machine_name":"machine","coordinator_model":"model","run_id":"run","reviewers":[]} {}`,
			dst:  &CreateReviewRequest{},
		},
		{
			name: "friction",
			body: `{"machine_name":"machine","coordinator_model":"model","category":"tooling","summary":"summary"} null`,
			dst:  &CreateFrictionRequest{},
		},
		{
			name: "event",
			body: `{"kind":"deploy","key":"k","machine_name":"m","coordinator_model":"c","payload":{}} 1`,
			dst:  &CreateEventRequest{},
		},
		{
			name: "processed",
			body: `{"ids":[1]} []`,
			dst:  &SetProcessedRequest{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc.body))

			if decodeRequestBody(rec, req, tc.dst) {
				t.Fatal("expected a trailing JSON value to be rejected")
			}
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected status %d, got %d", http.StatusBadRequest, rec.Code)
			}
		})
	}
}

func TestDecodeRequestBodyCountsTrailingWhitespace(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(
		http.MethodPost,
		"/",
		strings.NewReader(`{"ids":[1]}`+strings.Repeat(" ", maxRequestBytes)),
	)

	var body SetProcessedRequest
	if decodeRequestBody(rec, req, &body) {
		t.Fatal("expected a body over the limit only because of trailing whitespace to be rejected")
	}
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected status %d, got %d", http.StatusRequestEntityTooLarge, rec.Code)
	}
}

func TestDecodeRequestBodyRejectsUnknownFields(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"ids":[1],"unexpected":true}`))

	var body SetProcessedRequest
	if decodeRequestBody(rec, req, &body) {
		t.Fatal("expected unknown JSON field to be rejected")
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d", http.StatusBadRequest, rec.Code)
	}
}
