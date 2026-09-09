package released_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/RenseiAI/donmai/daemon"
	"github.com/RenseiAI/donmai/executioncell"
	"github.com/RenseiAI/donmai/runner"
)

func TestReleasedNackRejectedWorkPostsOneLocalRequest(t *testing.T) {
	typedDenial := &runner.HarnessAdmissionError{
		Code:    executioncell.DenialFallbackNotAllowed,
		Harness: "local-harness",
		Detail:  "the admitted harness cannot fall back",
	}
	tests := []struct {
		name             string
		acceptErr        error
		wantTypedReason  bool
		wantLegacyObject bool
	}{
		{
			name:             "generic rejection keeps legacy body",
			acceptErr:        errors.New("local admission failed"),
			wantLegacyObject: true,
		},
		{
			name:             "identical generic prose keeps legacy body",
			acceptErr:        errors.New(typedDenial.Error()),
			wantLegacyObject: true,
		},
		{
			name:            "wrapped typed fallback denial carries closed reason",
			acceptErr:       fmt.Errorf("accept work: %w", typedDenial),
			wantTypedReason: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			item := &daemon.PollWorkItem{
				SessionID:       "session-consumer-smoke",
				IssueID:         "issue-consumer-smoke",
				IssueIdentifier: "SMOKE-1",
				Repository:      "local/repository",
				Priority:        7,
				QueuedAt:        1777658441780,
			}

			var requests int
			var requestMethod, requestPath, requestAuth, requestContentType string
			var requestBody map[string]json.RawMessage
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				requestMethod = r.Method
				requestPath = r.URL.Path
				requestAuth = r.Header.Get("Authorization")
				requestContentType = r.Header.Get("Content-Type")
				var err error
				requestBody, err = readNackBody(r)
				if err != nil {
					t.Errorf("decode request body: %v", err)
					return
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			t.Cleanup(server.Close)

			err := daemon.NackRejectedWork(
				context.Background(),
				server.Client(),
				server.URL,
				"worker-consumer-smoke",
				"runtime-token-consumer-smoke",
				item,
				tt.acceptErr,
			)
			if err != nil {
				t.Fatalf("NackRejectedWork: %v", err)
			}
			if requests != 1 {
				t.Fatalf("request count = %d, want exactly one", requests)
			}
			if requestMethod != http.MethodPost {
				t.Errorf("method = %q, want POST", requestMethod)
			}
			if requestPath != "/api/sessions/"+item.SessionID+"/nack" {
				t.Errorf("path = %q, want /api/sessions/%s/nack", requestPath, item.SessionID)
			}
			if requestAuth != "Bearer runtime-token-consumer-smoke" {
				t.Errorf("Authorization = %q, want caller runtime bearer", requestAuth)
			}
			if requestContentType != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", requestContentType)
			}

			var body struct {
				WorkerID string               `json:"workerId"`
				Reason   string               `json:"reason"`
				Work     *daemon.PollWorkItem `json:"work"`
			}
			if err := decodeNackBody(requestBody, &body); err != nil {
				t.Fatalf("decode typed NACK body: %v", err)
			}
			if body.WorkerID != "worker-consumer-smoke" {
				t.Errorf("workerId = %q, want caller worker", body.WorkerID)
			}
			if body.Reason != "accept work failed: "+tt.acceptErr.Error() {
				t.Errorf("reason = %q, want compatibility rejection prose", body.Reason)
			}
			var gotWork, wantWork map[string]json.RawMessage
			if err := json.Unmarshal(requestBody["work"], &gotWork); err != nil {
				t.Fatalf("decode wire work: %v", err)
			}
			encodedItem, err := json.Marshal(item)
			if err != nil {
				t.Fatalf("encode original work: %v", err)
			}
			if err := json.Unmarshal(encodedItem, &wantWork); err != nil {
				t.Fatalf("decode original work: %v", err)
			}
			if !reflect.DeepEqual(gotWork, wantWork) {
				t.Errorf("wire work = %#v, want original item JSON %#v", gotWork, wantWork)
			}

			var reason map[string]string
			if raw, ok := requestBody["receiptPreflightReason"]; ok {
				if err := json.Unmarshal(raw, &reason); err != nil {
					t.Fatalf("decode typed receipt reason: %v", err)
				}
			}
			if tt.wantTypedReason {
				want := map[string]string{
					"contractVersion": "receipt-preflight-nack-reason/v1",
					"code":            string(executioncell.DenialFallbackNotAllowed),
				}
				if !reflect.DeepEqual(reason, want) {
					t.Errorf("receiptPreflightReason = %#v, want %#v", reason, want)
				}
			} else if tt.wantLegacyObject {
				if _, ok := requestBody["receiptPreflightReason"]; ok {
					t.Errorf("generic rejection unexpectedly carried typed receipt reason")
				}
				if len(requestBody) != 3 {
					t.Errorf("generic NACK fields = %d, want legacy three-field body", len(requestBody))
				}
			}
		})
	}
}

func readNackBody(r *http.Request) (map[string]json.RawMessage, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, err
	}
	return decoded, nil
}

func decodeNackBody(body map[string]json.RawMessage, dst any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	return json.Unmarshal(encoded, dst)
}
