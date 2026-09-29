package web

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"nori/internal/store"
)

func TestOAuthRegistrationHeadingAlignsWithFirstColumn(t *testing.T) {
	browser := layoutTestBrowser(t)
	now := time.Date(2026, time.September, 18, 9, 45, 0, 0, time.UTC)
	view := mcpSettingsView{Registrations: []store.OAuthRegistration{{
		ClientName: "Codex",
		ApprovedAt: now,
		ExpiresAt:  now.Add(30 * 24 * time.Hour),
		Grants: []store.OAuthGrant{{
			ManagementID: "layout-test-grant",
			ClientName:   "Codex",
			Scopes:       "nori:read nori:write",
			ApprovedAt:   now,
			ExpiresAt:    now.Add(30 * 24 * time.Hour),
			Status:       store.OAuthGrantActive,
		}},
	}}}

	var page bytes.Buffer
	if err := SettingsPage("Nori", "csrf", "", false, store.MCPConfig{Enabled: true}, view, Channels{}, nil).Render(context.Background(), &page); err != nil {
		t.Fatal(err)
	}
	probe := `<script>
		addEventListener('load', () => requestAnimationFrame(() => {
			const textLeft = (element) => {
				const range = document.createRange();
				range.selectNodeContents(element);
				return range.getBoundingClientRect().left;
			};
			const headingLeft = textLeft(document.querySelector('.oauth-registration-heading h3'));
			const columnLeft = textLeft(document.querySelector('.oauth-grant-table th'));
			const delta = Math.abs(headingLeft - columnLeft);
			document.body.dataset.oauthAlignment = delta < 1 ? 'aligned' : 'misaligned:' + delta.toFixed(2);
		}));
	</script>`
	html := strings.Replace(page.String(), "</body>", probe+"</body>", 1)
	css, err := staticFS.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/settings", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(html))
	})
	mux.HandleFunc("/static/app.css", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		_, _ = w.Write(css)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	cmd := exec.Command(browser,
		"--headless=new",
		"--no-sandbox",
		"--disable-gpu",
		"--dump-dom",
		"--virtual-time-budget=1000",
		"--window-size=1280,800",
		server.URL+"/settings",
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	dom, err := cmd.Output()
	if err != nil {
		t.Fatalf("run headless browser: %v\n%s", err, stderr.String())
	}
	match := regexp.MustCompile(`data-oauth-alignment="([^"]+)"`).FindSubmatch(dom)
	if len(match) != 2 {
		t.Fatalf("browser did not report OAuth alignment\n%s", dom)
	}
	if got := string(match[1]); got != "aligned" {
		t.Fatalf("OAuth client heading and first table column are %s", got)
	}
}

func layoutTestBrowser(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"chromium", "chromium-browser", "google-chrome", "google-chrome-stable"} {
		if path, err := exec.LookPath(name); err == nil {
			return path
		}
	}
	message := fmt.Sprintf("Chromium-family browser is required for layout regression tests (PATH=%s)", os.Getenv("PATH"))
	if os.Getenv("CI") != "" {
		t.Fatal(message)
	}
	t.Skip(message)
	return ""
}
