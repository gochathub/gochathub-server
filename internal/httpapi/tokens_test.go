package httpapi_test

import (
	"strings"
	"testing"
)

// TestAPITokens: a session mints a token → the token authenticates → the list
// never shows the secret → tokens cannot mint tokens → only the owner revokes.
func TestAPITokens(t *testing.T) {
	url := testURL(t)
	ts, svc := newServer(t, url, nil)
	seedUsers(t, svc, "tok", "tok2")

	anon := &client{t: t, b: ts.URL}
	login := func(name string) *client {
		in := map[string]any{"username": name, "password": "pw-" + name, "token_request": true}
		return &client{t: t, b: ts.URL, tk: anon.str(anon.do("POST", "/api/v1/auth/login", in, 200), "token")}
	}
	c, other := login("tok"), login("tok2")

	minted := c.do("POST", "/api/v1/users/me/tokens", map[string]string{"name": "phone"}, 201)
	id, raw := c.str(minted, "id"), c.str(minted, "token")
	api := &client{t: t, b: ts.URL, tk: raw}
	api.do("GET", "/api/v1/users/me", nil, 200)

	list := c.doArr("GET", "/api/v1/users/me/tokens", nil, 200)
	if len(list) != 1 {
		t.Fatalf("list = %v", list)
	}
	item := list[0].(map[string]any)
	if item["id"] != id || item["name"] != "phone" || item["token"] != nil {
		t.Fatalf("list item = %v", item)
	}

	api.do("POST", "/api/v1/users/me/tokens", map[string]string{"name": "x"}, 403) // tokens cannot mint tokens
	c.do("POST", "/api/v1/users/me/tokens", map[string]string{"name": strings.Repeat("x", 101)}, 400)

	other.do("DELETE", "/api/v1/users/me/tokens/"+id, nil, 404) // not theirs
	api.do("GET", "/api/v1/users/me", nil, 200)

	c.do("DELETE", "/api/v1/users/me/tokens/"+id, nil, 204)
	api.do("GET", "/api/v1/users/me", nil, 401)
	if n := len(c.doArr("GET", "/api/v1/users/me/tokens", nil, 200)); n != 0 {
		t.Fatalf("revoked token still listed (%d)", n)
	}
}
