//go:build securityreview

// Security-review probes (independent reviewer, 2026-11-09). These assert
// the SECURE behaviour and currently FAIL, documenting open findings. They
// are behind the `securityreview` build tag so the default suite stays
// green:
//
//	go test -race -tags securityreview -run SecReview ./services/central-api/internal/server/
package server

import (
	"net/http"
	"testing"
)

// Four-eyes rule: the reviewer must not be an author of any content in the
// version being verified. Today only the LAST editor is compared, so author
// A's content is self-verified once any other editor saves a trivial
// follow-up draft on top of it.
func TestSecReviewFourEyesBypassViaIntermediateEdit(t *testing.T) {
	e := newEnv(t)
	e.createUser("alice", "scheme_editor", "scheme_reviewer") // author + reviewer
	e.createUser("bob", "scheme_editor")                      // any second editor
	a, b := e.adminClient(), e.adminClient()
	a.login("alice")
	b.login("bob")
	path := "/v1/admin/schemes/test-scheme-fe"

	// v1: alice writes the content (would contain the facts she wants published).
	rec := fixtureRecord("test-scheme-fe")
	rec.Summary.En = "Software test fixture only (alice content)."
	if code, body, _ := a.req("PUT", path, putBody(0, rec)); code != 200 {
		t.Fatalf("alice draft: %d %s", code, body)
	}
	// v2: bob makes a trivial change on top (alice's content carried over).
	if code, body, _ := b.req("PUT", path, map[string]any{"base_version": 1, "change_note": "typo", "record": rec}); code != 200 {
		t.Fatalf("bob trivial edit: %d %s", code, body)
	}
	// alice verifies v2, i.e. her own content.
	code, body, _ := a.req("POST", path+"/verify", map[string]any{"version": 2})
	if code != http.StatusForbidden {
		t.Fatalf("FINDING: author of v1 content verified it via v2 (status %d): %s", code, body)
	}
}

// Login must not reveal which usernames exist. A locked existing account
// answers 429 "locked" while an unknown username answers 401.
func TestSecReviewLockoutRevealsUsernames(t *testing.T) {
	e := newEnv(t)
	e.createUser("realuser", "viewer")
	c := e.adminClient()
	for i := 0; i < maxFailedLogins; i++ {
		c.req("POST", "/v1/admin/login", map[string]string{"username": "realuser", "password": "wrong-password-xx"})
	}
	codeReal, _, _ := c.req("POST", "/v1/admin/login", map[string]string{"username": "realuser", "password": "wrong-password-xx"})
	codeFake, _, _ := c.req("POST", "/v1/admin/login", map[string]string{"username": "nosuchuser", "password": "wrong-password-xx"})
	if codeReal != codeFake {
		t.Fatalf("FINDING: existing locked user -> %d, unknown user -> %d (username enumeration)", codeReal, codeFake)
	}
}

// Ten unauthenticated wrong guesses lock ANY known account, including the
// legitimate owner who then cannot log in with the right password.
func TestSecReviewLockoutDoSOfLegitimateUser(t *testing.T) {
	e := newEnv(t)
	e.createUser("victim", "admin")
	attacker := e.adminClient()
	for i := 0; i < maxFailedLogins; i++ {
		attacker.req("POST", "/v1/admin/login", map[string]string{"username": "victim", "password": "attacker-guess-1"})
	}
	owner := e.adminClient()
	code, body, _ := owner.req("POST", "/v1/admin/login", map[string]string{"username": "victim", "password": "correct-horse-victim"})
	if code != http.StatusOK {
		t.Fatalf("FINDING: legitimate admin locked out by anonymous guesses (status %d): %s", code, body)
	}
}
