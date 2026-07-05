package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Kodiqa-Solutions/VaultS3/internal/metadata"
)

// Regression for issue #23: the IAM attach/add actions previously returned 200
// with an empty body, and the dashboard's apiFetch called res.json() on it,
// throwing on Safari ("The string did not match the expected pattern"). These
// no-body actions must return 204 No Content.

func TestAttachUserPolicyReturns204(t *testing.T) {
	h, store := newTestAPI(t)
	if err := store.CreateIAMUser(metadata.IAMUser{Name: "alice"}); err != nil {
		t.Fatalf("CreateIAMUser: %v", err)
	}
	if err := store.CreateIAMPolicy(metadata.IAMPolicy{Name: "readonly", Document: `{"Statement":[{"Effect":"Allow","Action":["s3:*"],"Resource":["*"]}]}`}); err != nil {
		t.Fatalf("CreateIAMPolicy: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"policyName":"readonly"}`))
	rr := httptest.NewRecorder()
	h.handleAttachUserPolicy(rr, req, "alice")

	if rr.Code != http.StatusNoContent {
		t.Fatalf("attach policy: expected 204, got %d (body %q)", rr.Code, rr.Body.String())
	}
	if rr.Body.Len() != 0 {
		t.Fatalf("204 response must have an empty body, got %q", rr.Body.String())
	}
	u, err := store.GetIAMUser("alice")
	if err != nil || len(u.PolicyARNs) != 1 || u.PolicyARNs[0] != "readonly" {
		t.Fatalf("policy was not attached: %+v (err %v)", u, err)
	}

	// Re-attaching the same policy (the duplicate-guard path) must also be 204.
	rr2 := httptest.NewRecorder()
	h.handleAttachUserPolicy(rr2, httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"policyName":"readonly"}`)), "alice")
	if rr2.Code != http.StatusNoContent {
		t.Fatalf("re-attach: expected 204, got %d", rr2.Code)
	}
}

func TestAddUserToGroupReturns204(t *testing.T) {
	h, store := newTestAPI(t)
	if err := store.CreateIAMUser(metadata.IAMUser{Name: "bob"}); err != nil {
		t.Fatalf("CreateIAMUser: %v", err)
	}
	if err := store.CreateIAMGroup(metadata.IAMGroup{Name: "devs"}); err != nil {
		t.Fatalf("CreateIAMGroup: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"groupName":"devs"}`))
	rr := httptest.NewRecorder()
	h.handleAddUserToGroup(rr, req, "bob")

	if rr.Code != http.StatusNoContent {
		t.Fatalf("add to group: expected 204, got %d (body %q)", rr.Code, rr.Body.String())
	}
	if rr.Body.Len() != 0 {
		t.Fatalf("204 response must have an empty body, got %q", rr.Body.String())
	}
	u, err := store.GetIAMUser("bob")
	if err != nil || len(u.Groups) != 1 || u.Groups[0] != "devs" {
		t.Fatalf("user was not added to group: %+v (err %v)", u, err)
	}
}
