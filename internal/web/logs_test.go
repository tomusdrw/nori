package web

import (
	"context"
	"html"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"nori/internal/docker"
)

func TestReadLogLinesFormatsJSON(t *testing.T) {
	for _, tc := range []struct{ name, input, want string }{
		{"common fields", `{"msg":"request complete","level":"info","time":"2026-09-18T12:00:00Z","status":200,"path":"/health"}`, `2026-09-18T12:00:00Z INFO request complete path="/health" status=200`},
		{"aliases", `{"message":"ready","severity":"warn","timestamp":"2026-09-18T12:00:00Z"}`, `2026-09-18T12:00:00Z WARN ready`},
		{"nested data and precision", `{"msg":"result","request":{"id":9007199254740993},"items":[1, true],"duration":1.2300,"error":null}`, `result duration=1.2300 error=null items=[1,true] request={"id":9007199254740993}`},
		{"numeric metadata", `{"msg":"ready","level":30,"ts":1726660800.125}`, `ready level=30 ts=1726660800.125`},
		{"other fields", `{"port":8080,"host":"localhost"}`, `host="localhost" port=8080`},
		{"alternate fields retained", `{"msg":"first","message":"second","time":"now","ts":"later"}`, `now first message="second" ts="later"`},
		{"non-string and empty fields", `{"msg":{"error":"failed"},"level":null,"time":""}`, `level=null msg={"error":"failed"} time=""`},
		{"escaped line breaks", `{"msg":"first\nsecond\tthird","level":"info"}`, `INFO "first\nsecond\tthird"`},
		{"unusual key", `{"two words":"value","line\nbreak":true}`, `"line\nbreak"=true "two words"="value"`},
		{"plain text", "  plain <text> & spaces  ", "  plain <text> & spaces  "},
		{"malformed JSON", `{"msg":"unfinished"`, `{"msg":"unfinished"`},
		{"trailing text", `{"msg":"ready"} suffix`, `{"msg":"ready"} suffix`},
		{"array", `[1, "two"]`, `[1, "two"]`},
		{"null", `null`, `null`},
		{"empty object", `{}`, `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := readLogLines(strings.NewReader(tc.input + "\n"))
			if len(got) != 1 || got[0] != tc.want {
				t.Fatalf("readLogLines = %q, want [%q]", got, []string{tc.want})
			}
		})
	}
}

func TestJSONLogsInDashboardAndStream(t *testing.T) {
	const input = "older line\n{\"level\":\"info\",\"msg\":\"ready <script>alert(1)</script>\",\"port\":8080}\nplain <text>\n{invalid json}\n"
	const formatted = "INFO ready <script>alert(1)</script> port=8080\nplain <text>\n{invalid json}"
	cs := []docker.Container{{ID: "web", Name: "app-web"}}
	srv := &Server{docker: &docker.Fake{
		Containers: map[string][]docker.Container{"app": cs},
		LogData:    map[string]string{"web": input},
	}}
	preview := srv.recentLogs(context.Background(), cs, 3)
	if preview != formatted {
		t.Fatalf("preview = %q, want %q", preview, formatted)
	}
	page := httptest.NewRecorder()
	if err := Page([]ServiceView{{Name: "app", RecentLogs: preview}}, "").Render(context.Background(), page); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(page.Body.String(), html.EscapeString(formatted)) {
		t.Fatalf("dashboard missing escaped formatted logs: %s", page.Body.String())
	}
	router := chi.NewRouter()
	router.Get("/services/{name}/logs/stream", srv.handleLogsStream)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, httptest.NewRequest("GET", "/services/app/logs/stream", nil))
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), html.EscapeString(formatted)) {
		t.Fatalf("stream: status %d, body %q, want it to contain %q", rr.Code, rr.Body.String(), html.EscapeString(formatted))
	}
	if strings.Contains(rr.Body.String(), "===") {
		t.Fatalf("single-container stream should not label containers: %s", rr.Body.String())
	}
}

func TestLogsStreamContainerTabs(t *testing.T) {
	srv := &Server{docker: &docker.Fake{
		Containers: map[string][]docker.Container{"app": {
			{ID: "web", Name: "app-web"},
			{ID: "worker", Name: "app-worker"},
		}},
		LogData: map[string]string{
			"web":    "web line\n",
			"worker": "worker line\n",
		},
	}}
	router := chi.NewRouter()
	router.Get("/services/{name}/logs/stream", srv.handleLogsStream)

	for _, tc := range []struct {
		name               string
		query              string
		wantLog, wantTab   string
		otherLog, otherTab string
	}{
		{"defaults to first container", "", "web line", "app-web", "worker line", "app-worker"},
		{"selects requested container", "?container=app-worker", "worker line", "app-worker", "web line", "app-web"},
		{"falls back on unknown container", "?container=nope", "web line", "app-web", "worker line", "app-worker"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			router.ServeHTTP(rr, httptest.NewRequest("GET", "/services/app/logs/stream"+tc.query, nil))
			body := rr.Body.String()
			if rr.Code != 200 || !strings.Contains(body, html.EscapeString(tc.wantLog)) || strings.Contains(body, tc.otherLog) {
				t.Fatalf("stream: status %d, body %q", rr.Code, body)
			}
			if !strings.Contains(body, `id="container-logs"`) {
				t.Fatalf("stream missing fragment wrapper: %s", body)
			}
			active := `<button class="log-tab active" aria-selected="true" hx-get="/services/app/logs/stream?container=` + tc.wantTab + `"`
			if !strings.Contains(body, active) {
				t.Fatalf("stream missing active tab for %s: %s", tc.wantTab, body)
			}
			inactive := `<button class="log-tab" aria-selected="false" hx-get="/services/app/logs/stream?container=` + tc.otherTab + `"`
			if !strings.Contains(body, inactive) {
				t.Fatalf("stream missing tab for %s: %s", tc.otherTab, body)
			}
		})
	}

	t.Run("no containers shows placeholder without tabs", func(t *testing.T) {
		empty := &Server{docker: &docker.Fake{Containers: map[string][]docker.Container{"app": nil}}}
		r := chi.NewRouter()
		r.Get("/services/{name}/logs/stream", empty.handleLogsStream)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, httptest.NewRequest("GET", "/services/app/logs/stream", nil))
		if rr.Code != 200 || !strings.Contains(rr.Body.String(), "No container logs yet.") || strings.Contains(rr.Body.String(), "log-tab") {
			t.Fatalf("empty stream: status %d, body %q", rr.Code, rr.Body.String())
		}
	})
}

func TestServiceDetailLogsPanel(t *testing.T) {
	data := ServiceDetailData{Service: ServiceFormData{Name: "app", WatchedImage: "ghcr.io/app:1"}}
	page := httptest.NewRecorder()
	if err := ServiceDetailPage(data, "").Render(context.Background(), page); err != nil {
		t.Fatal(err)
	}
	body := page.Body.String()
	for _, want := range []string{
		`id="container-logs"`,
		`hx-get="/services/app/logs/stream"`,
		`hx-swap="outerHTML"`,
		"The latest 100 lines from the selected container.",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("detail page missing %q: %s", want, body)
		}
	}
}
