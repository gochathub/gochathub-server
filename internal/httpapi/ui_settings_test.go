package httpapi_test

import (
	"context"
	"testing"
)

// TestUISettings: admin-only default layout; users see it as instance_layout
// and follow it while their own layout is "instance" (ADR-027).
func TestUISettings(t *testing.T) {
	url := testURL(t)
	ts, svc := newServer(t, url, nil)
	seedUsers(t, svc, "plain")
	if _, err := svc.Users.CreateUser(context.Background(), "root", "Root", "root@test.invalid", "pw-root", "admin"); err != nil {
		t.Fatal(err)
	}
	anon := &client{t: t, b: ts.URL}
	admin := anon.forUser(svc, "root")
	plain := anon.forUser(svc, "plain")
	const path = "/api/v1/admin/ui-settings"

	anon.do("GET", path, nil, 401)
	plain.do("GET", path, nil, 403)

	if v := admin.do("GET", path, nil, 200); v["default_layout"] != "modern" {
		t.Fatalf("fresh default: %v", v)
	}
	admin.do("PUT", path, map[string]string{"default_layout": "rocket"}, 400)
	admin.do("PUT", path, map[string]string{"default_layout": "instance"}, 400)
	if v := admin.do("PUT", path, map[string]string{"default_layout": "classic"}, 200); v["default_layout"] != "classic" {
		t.Fatalf("saved: %v", v)
	}
	if p := plain.do("GET", "/api/v1/users/me/preferences", nil, 200); p["layout"] != "instance" || p["instance_layout"] != "classic" {
		t.Fatalf("user prefs follow admin default: %v", p)
	}
}
