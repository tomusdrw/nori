package web

import (
	"context"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"nori/internal/auth"
	"nori/internal/docker"
	"nori/internal/executor"
	"nori/internal/poller"
	"nori/internal/store"
)

func TestDashboardServiceGroups(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "groups.db"), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, name := range []string{"api", "blog", "cache", "worker"} {
		if err := st.CreateService(ctx, &store.Service{Name: name, Policy: store.PolicyManual}); err != nil {
			t.Fatal(err)
		}
	}
	latest := func(context.Context, string) (string, error) { return "", nil }
	ex := executor.New(st, &executor.OSRunner{}, latest, 0)
	hash, _ := auth.HashPassword("test")
	a, _ := auth.New(hash, make([]byte, 32))
	srv := NewServer(st, &docker.Fake{}, ex, poller.New(st, latest, ex, 0), a, Channels{})
	cookies := loginCookies(t, srv)
	csrf := csrfFromBody(getAuthed(t, srv, cookies, "/"))
	post := func(path string, form url.Values) {
		t.Helper()
		if form == nil {
			form = url.Values{}
		}
		form.Set("csrf_token", csrf)
		postAuthed(t, srv, cookies, path, form.Encode())
	}
	assertOrder := func(path string, markers ...string) {
		t.Helper()
		body := getAuthed(t, srv, cookies, path)
		for _, marker := range markers {
			index := strings.Index(body, marker)
			if index < 0 {
				t.Fatalf("GET %s missing or out of order %q", path, marker)
			}
			body = body[index+len(marker):]
		}
	}

	form := getAuthed(t, srv, cookies, "/services/new")
	if !strings.Contains(form, `name="group"`) || !strings.Contains(form, `value="Ungrouped"`) {
		t.Fatal("service form is missing group suggestions")
	}
	create := func(name, group string) {
		post("/services", url.Values{
			"name": {name}, "group": {group}, "deployment_mode": {"manual_script"}, "policy": {"manual"}, "deploy_script": {"true"},
		})
	}
	create("gateway", "Platform & Infra")
	create("publisher", "Publishing")
	create("proxy", "platform & infra")
	assertOrder("/", ">Ungrouped<", ">api</a>", ">Platform &amp; Infra<", ">gateway</a>", ">proxy</a>", ">Publishing<", ">publisher</a>")

	edit := getAuthed(t, srv, cookies, "/services/gateway/edit")
	if !strings.Contains(edit, `value="Platform &amp; Infra"`) {
		t.Fatal("edit form is missing the saved group")
	}
	post("/services/gateway", url.Values{
		"group": {""}, "expected_config_version": {"1"}, "deployment_mode": {"manual_script"}, "policy": {"manual"}, "deploy_script": {"true"},
	})
	assertOrder("/", ">Ungrouped<", ">gateway</a>", ">Platform &amp; Infra<", ">proxy</a>")

	post("/service-groups/1/move", url.Values{"target": {"3"}, "before": {"false"}})
	assertOrder("/partials/services", ">Platform &amp; Infra<", ">Publishing<", ">Ungrouped<")
	post("/service-groups/1/fold", url.Values{"collapsed": {"true"}})
	body := getAuthed(t, srv, cookies, "/partials/services")
	if strings.Contains(body, ">gateway</a>") || !strings.Contains(body, "5 without containers") {
		t.Fatal("folded default group should show a status summary instead of cards")
	}
	post("/service-groups/1/fold", url.Values{"collapsed": {"false"}})
	assertOrder("/partials/services", ">Ungrouped<", ">gateway</a>")
}
