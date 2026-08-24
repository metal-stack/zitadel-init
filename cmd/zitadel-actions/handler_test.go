package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zitadel/zitadel-go/v3/pkg/actions"
)

func TestHandler(t *testing.T) {
	const testSigningKey = "test-signing-key"

	var (
		signedRequest = func(t *testing.T, body string) *http.Request {
			t.Helper()

			req := httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set(actions.SigningHeader, actions.ComputeSignatureHeader(time.Now(), []byte(body), testSigningKey))
			return req
		}

		payloadBody = func(roles []string, clientID string) string {
			b, _ := json.Marshal(actionPayload{
				Function: "function/preuserinfo",
				Userinfo: struct {
					Roles []string `json:"roles"`
				}{Roles: roles},
				Application: struct {
					ClientID string `json:"client_id"`
				}{ClientID: clientID},
			})

			return string(b)
		}
	)

	tests := []struct {
		name         string
		allowedRoles []string
		clientIDs    []string
		body         string
		buildReq     func(*testing.T, string) *http.Request
		wantStatus   int
		wantBody     string
	}{
		{
			name:         "allowed roles present",
			allowedRoles: []string{"admin"},
			clientIDs:    nil,
			body:         payloadBody([]string{"admin", "user"}, "app-a"),
			buildReq:     signedRequest,
			wantStatus:   http.StatusOK,
			wantBody:     payloadBody([]string{"admin", "user"}, "app-a"),
		},
		{
			name:         "no allowed role present",
			allowedRoles: []string{"admin"},
			clientIDs:    nil,
			body:         payloadBody([]string{"user"}, "app-a"),
			buildReq:     signedRequest,
			wantStatus:   http.StatusOK,
			wantBody:     `{"forwardedStatusCode":403,"forwardedErrorMessage":"login not allowed because none of the following roles are present: [\"admin\"]"}`,
		},
		{
			name:         "multiple allowed roles, one matches",
			allowedRoles: []string{"admin", "superuser"},
			clientIDs:    nil,
			body:         payloadBody([]string{"superuser"}, "app-a"),
			buildReq:     signedRequest,
			wantStatus:   http.StatusOK,
			wantBody:     payloadBody([]string{"superuser"}, "app-a"),
		},
		{
			name:         "other app gets ignored",
			allowedRoles: []string{"admin"},
			clientIDs:    []string{"app-a"},
			body:         payloadBody([]string{"user"}, "app-b"),
			buildReq:     signedRequest,
			wantStatus:   http.StatusOK,
			wantBody:     payloadBody([]string{"user"}, "app-b"),
		},
		{
			name:         "allowed role with proper client id",
			allowedRoles: []string{"admin"},
			clientIDs:    []string{"app-a"},
			body:         payloadBody([]string{"admin"}, "app-a"),
			buildReq:     signedRequest,
			wantStatus:   http.StatusOK,
			wantBody:     payloadBody([]string{"admin"}, "app-a"),
		},
		{
			name:         "no allowed role with proper client id",
			allowedRoles: []string{"admin"},
			clientIDs:    []string{"app-a"},
			body:         payloadBody([]string{"user"}, "app-a"),
			buildReq:     signedRequest,
			wantStatus:   http.StatusOK,
			wantBody:     `{"forwardedStatusCode":403,"forwardedErrorMessage":"login not allowed because none of the following roles are present: [\"admin\"]"}`,
		},
		{
			name:         "no roles at all, no client scoping",
			allowedRoles: []string{"admin"},
			clientIDs:    nil,
			body:         payloadBody(nil, "app-a"),
			buildReq:     signedRequest,
			wantStatus:   http.StatusOK,
			wantBody:     `{"forwardedStatusCode":403,"forwardedErrorMessage":"login not allowed because none of the following roles are present: [\"admin\"]"}`,
		},
		{
			name:         "missing signature header",
			allowedRoles: []string{"admin"},
			clientIDs:    nil,
			body:         payloadBody([]string{"admin"}, "app-a"),
			buildReq: func(_ *testing.T, body string) *http.Request {
				return httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(body))
			},
			wantStatus: http.StatusBadRequest,
			wantBody:   "no Zitadel-Signature header set on request",
		},
		{
			name:         "invalid signature",
			allowedRoles: []string{"admin"},
			clientIDs:    nil,
			body:         payloadBody([]string{"admin"}, "app-a"),
			buildReq: func(_ *testing.T, body string) *http.Request {
				req := httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(body))
				req.Header.Set(actions.SigningHeader, "t=1,v1=deadbeef")
				return req
			},
			wantStatus: http.StatusBadRequest,
			wantBody:   "request signature timestamp is outside the allowed tolerance",
		},
		{
			name:         "invalid json body",
			allowedRoles: []string{"admin"},
			clientIDs:    nil,
			body:         `{"function":`,
			buildReq:     signedRequest,
			wantStatus:   http.StatusBadRequest,
			wantBody:     "unable to parse action payload: unexpected end of JSON input",
		},
		{
			name:         "non-post method",
			allowedRoles: []string{"admin"},
			clientIDs:    nil,
			body:         payloadBody([]string{"admin"}, "app-a"),
			buildReq: func(_ *testing.T, body string) *http.Request {
				req := httptest.NewRequest(http.MethodGet, "/", bytes.NewBufferString(body))
				req.Header.Set(actions.SigningHeader, actions.ComputeSignatureHeader(time.Now(), []byte(body), testSigningKey))
				return req
			},
			wantStatus: http.StatusMethodNotAllowed,
			wantBody:   "method not allowed, expected POST",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var (
				h = &handler{
					log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
					allowedRoles: tt.allowedRoles,
					clientIDs:    tt.clientIDs,
					signingKey:   testSigningKey,
				}

				rec = httptest.NewRecorder()
				req = tt.buildReq(t, tt.body)
			)

			h.handle(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body: %s)", rec.Code, tt.wantStatus, rec.Body.String())
			}

			if got := strings.TrimSuffix(rec.Body.String(), "\n"); got != tt.wantBody {
				t.Fatalf("response body = %q, want %q", got, tt.wantBody)
			}
		})
	}
}
