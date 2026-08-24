package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/zitadel/zitadel-go/v3/pkg/actions"
)

type handler struct {
	log          *slog.Logger
	allowedRoles []string
	clientIDs    []string
	signingKey   string
}

type blockResponse struct {
	ForwardedStatusCode   int    `json:"forwardedStatusCode"`
	ForwardedErrorMessage string `json:"forwardedErrorMessage"`
}

type actionPayload struct {
	Function string `json:"function"`
	Userinfo struct {
		Roles []string `json:"roles"`
	} `json:"userinfo"`
	Application struct {
		ClientID string `json:"client_id"`
	} `json:"application"`
}

func (h *handler) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		h.log.Warn("rejected request with non-POST method", "method", r.Method, "remote", r.RemoteAddr)
		http.Error(w, "method not allowed, expected POST", http.StatusMethodNotAllowed)
		return
	}

	payload, err := io.ReadAll(r.Body)
	if err != nil {
		h.log.Error("unable to read request body", "error", err)
		http.Error(w, fmt.Sprintf("unable to read request body: %v", err), http.StatusInternalServerError)
		return
	}

	if err := actions.ValidateRequestPayload(payload, &r.Header, h.signingKey); err != nil {
		msg := "invalid signature"
		status := http.StatusBadRequest

		switch {
		case errors.Is(err, actions.ErrMissingHeader):
			msg = "missing Zitadel-Signature header"
		case errors.Is(err, actions.ErrNotSigned):
			msg = "no Zitadel-Signature header set on request"
		case errors.Is(err, actions.ErrInvalidHeader):
			msg = "malformed Zitadel-Signature header"
		case errors.Is(err, actions.ErrNoValidSignature):
			msg = "request signature does not match"
		case errors.Is(err, actions.ErrTooOld):
			msg = "request signature timestamp is outside the allowed tolerance"
		}

		h.log.Warn("rejected request with invalid signature", "error", err, "msg", msg, "remote", r.RemoteAddr)
		http.Error(w, msg, status)
		return
	}

	var p actionPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		h.log.Error("unable to parse action payload", "error", err)
		http.Error(w, fmt.Sprintf("unable to parse action payload: %v", err), http.StatusBadRequest)
		return
	}

	h.log.Info("received authenticated action request", "function", p.Function, "roles", p.Userinfo.Roles, "client_id", p.Application.ClientID)

	if len(h.clientIDs) > 0 && !slices.Contains(h.clientIDs, p.Application.ClientID) {
		h.log.Info("skipping role policy for application", "client_id", p.Application.ClientID)

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(payload)

		return
	}

	for _, role := range h.allowedRoles {
		if slices.Contains(p.Userinfo.Roles, role) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(payload)
			return
		}
	}

	h.log.Info("blocking login because claims do not contain desired roles", "want", strings.Join(h.allowedRoles, ", "), "got", strings.Join(p.Userinfo.Roles, ", "))

	resp := blockResponse{
		ForwardedStatusCode:   http.StatusForbidden,
		ForwardedErrorMessage: fmt.Sprintf("login not allowed because none of the following roles are present: %q", h.allowedRoles),
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		h.log.Error("unable to write response", "error", err)
	}
}
