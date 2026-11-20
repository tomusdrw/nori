package web

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/a-h/templ"
	"nori/internal/backup"
	"nori/internal/store"
)

func layoutBackups() templ.Component {
	return backupPage(backupPageData{KeyAvailable: true, B2: backup.B2Config{Prefix: "nori"},
		Disks:      []backup.DiskUsage{{Path: "/data/backups", Status: "healthy", Available: 328 * backup.GiB, Total: 462 * backup.GiB}},
		DiskPolicy: backup.DiskPolicy{WarningPercent: 15, CriticalPercent: 5, WarningBytes: 5 * backup.GiB, CriticalBytes: backup.GiB},
	}, "csrf")
}

func layoutResources() templ.Component {
	data := resourcesOverview{
		services: resourceTable{title: "Storage by service", first: "Service", columns: storageMetrics},
		sources:  resourceTable{title: "Storage sources", first: "Source", context: "Type · Ownership", columns: storageMetrics},
		runtime:  resourceTable{title: "Runtime by service", first: "Service", columns: runtimeMetrics},
	}
	at := time.Date(2026, 11, 19, 9, 45, 0, 0, time.UTC)
	data.view.storage.at = at
	data.view.runtime.at = at
	for i, name := range []string{"website-creator-runner-2", "analytics-worker", "api", "web-staging"} {
		row := resourceRow{name: name, url: serviceResourcesURL(name), values: map[string]resourceValue{}}
		for key, value := range map[string]float64{"cpu": 2.8, "bytes": 3.7 * float64(backup.GiB), "memory": 429 << 20, "rx": 7680, "tx": 512, "read": 0, "write": 2048, "pids": 16} {
			row.values[key] = resourceValue{known: true, number: value / float64(i+1)}
		}
		row.values["limit"] = resourceValue{unlimited: true}
		row.values["rx"] = resourceValue{known: true, number: 7680, missing: []string{"logs"}}
		data.services.rows = append(data.services.rows, row)
		data.runtime.rows = append(data.runtime.rows, row)
		data.view.storage.all = append(data.view.storage.all, store.ResourceReading{Values: map[string]float64{"bytes": row.values["bytes"].number}})
		row.url = ""
		row.detail = "Writable layer · " + name
		data.sources.rows = append(data.sources.rows, row)
	}
	for _, table := range []*resourceTable{&data.services, &data.sources, &data.runtime} {
		table.rank("", "")
	}
	return resourcesPage(data, "csrf")
}

func layoutDashboard() templ.Component {
	api := ServiceView{Name: "gateway", State: "running", RunningFor: "3 days", LastDeploy: "3 days ago", Managed: true,
		RunningVersion: "da4f810", LatestVersion: "e83b7c1", UpdateAvailable: true, RecentLogs: "GET /health 200  2ms\nGET /api/projects 200  18ms"}
	worker := ServiceView{Name: "website-creator-background-worker", State: "running", RunningFor: "18 hours", LastDeploy: "Yesterday",
		RunningVersion: "918acfd", LatestVersion: "918acfd", RecentLogs: "Worker ready\nCompleted render job #1842"}
	batch := ServiceView{Name: "nightly-export", State: "none", ManualScript: true, RunningFor: "—", LastDeploy: "4 hours ago", RecentLogs: "No container logs yet."}
	return Page(dashboardView{Count: 4, Groups: []dashboardGroup{
		{ServiceGroup: store.ServiceGroup{ID: 2, Name: "Production"}, Services: []ServiceView{api, worker}},
		{ServiceGroup: store.ServiceGroup{ID: 3, Name: "Scheduled jobs"}, Services: []ServiceView{batch}},
		{ServiceGroup: store.ServiceGroup{ID: 1, Name: "Ungrouped", Collapsed: true}, Services: []ServiceView{{Name: "preview", State: "stopped"}}},
	}}, "csrf")
}

func TestFeatureLayouts(t *testing.T) {
	browser := layoutTestBrowser(t)
	static, err := fs.Sub(staticFS, "static")
	if err != nil {
		t.Fatal(err)
	}
	for _, page := range []struct {
		name, section string
		component     templ.Component
		probe         string
	}{
		{"settings", "Settings", SettingsPage("Test", "csrf", "", false, store.MCPConfig{}, mcpSettingsView{}, Channels{}, nil), `
			const matrix = document.querySelector('.notify-matrix-wrap');
			checkLayout(matrix.scrollWidth <= matrix.clientWidth + 1, 'notification controls fit');
		`},
		{"service-detail", "Services", ServiceDetailPage(ServiceDetailData{
			Service: ServiceFormData{Name: "website-creator-background-worker", WatchedImage: "ghcr.io/smallbeast/website-creator-background-worker:stable"},
			State:   "running", UpdateAvail: true, Containers: []ContainerView{{Name: "website-creator-background-worker-1", Image: "ghcr.io/smallbeast/website-creator-background-worker:stable", State: "running"}},
			Deployments: []DeploymentView{{ID: 184, Trigger: "scheduled", Status: "success", StartedAt: "2026-11-19 09:45 UTC"}},
		}, "csrf"), `
			for (const table of document.querySelectorAll('.table-wrap')) {
				checkLayout(table.scrollWidth <= table.clientWidth + 1, 'service table overflow');
			}
		`},
		{"services", "Services", layoutDashboard(), `
			const totals = [...document.querySelectorAll('.dashboard-summary dd')].map(item => item.textContent);
			checkLayout(totals.join('|') === '4|2|1', 'live service totals');
			const name = document.querySelectorAll('.service-name')[1];
			checkLayout(name.scrollWidth <= name.clientWidth + 1, 'full service name');
			const folded = document.querySelector('[aria-expanded="false"]');
			checkLayout(document.getElementById(folded.getAttribute('aria-controls')).hidden, 'folded group');
			for (const row of document.querySelectorAll('.service-card')) {
				checkLayout(row.scrollWidth <= row.clientWidth + 1, 'service row overflow');
			}
		`},
		{"backups", "Backups", layoutBackups(), `
			const form = document.querySelector('form[action="/backups/import"]');
			checkLayout([...new FormData(form).keys()].join('|') === 'csrf_token|identity|archive', 'streaming upload field order');
			const panel = document.querySelector('.backup-history');
			const heading = panel.querySelector('h2');
			const check = document.querySelector('[name=read_only]');
			checkLayout(rect(heading).left - rect(panel).left >= 16, 'heading inset');
			checkLayout(rect(check).width <= 24 && rect(check).height <= 24, 'checkbox size');
			const disk = document.querySelector('#disk-space'), b2 = document.querySelector('#backblaze');
			checkLayout(rect(b2).top - rect(disk).bottom >= 12 || rect(b2).left - rect(disk).right >= 12, 'panel gap');
		`},
		{"resources", "Resources", layoutResources(), `
			for (const wrap of document.querySelectorAll('.resource-panel .table-wrap')) {
				checkLayout(wrap.scrollWidth <= wrap.clientWidth + 1, 'columns overflow');
			}
			checkLayout(Array.from(document.querySelectorAll('.resource-panel tbody td')).every(td => rect(td).width > 0 && rect(td).right <= document.documentElement.clientWidth), 'metrics visible');
		`},
	} {
		var buf bytes.Buffer
		if err := page.component.Render(context.Background(), &buf); err != nil {
			t.Fatal(err)
		}
		probe := `<script>window.addEventListener('load',()=>{const rect=e=>e.getBoundingClientRect();const errors=[];const checkLayout=(ok,msg)=>{if(!ok)errors.push(msg)};` + page.probe + `
			checkLayout(document.documentElement.scrollWidth <= document.documentElement.clientWidth, 'page overflow');
			const links = [...document.querySelectorAll('nav[aria-label="Main navigation"] a')];
			checkLayout(links.map(a => a.textContent.trim()).join('|') === 'Services|Resources|Terminal|Backups|Settings', 'section labels');
			checkLayout(links.every(a => rect(a).width > 0 && rect(a).right <= document.documentElement.clientWidth), 'visible navigation');
			const active = links.filter(a => a.hasAttribute('aria-current'));
			checkLayout(active.length === 1 && active[0].textContent === ` + fmt.Sprintf("%q", page.section) + `, 'current section');
			const tips = [...document.querySelectorAll('button.help-trigger')];
			if (` + fmt.Sprint(page.name == "services" || page.name == "backups" || page.name == "resources") + `) checkLayout(tips.length > 0, 'section help available');
			for (const tip of tips) {
				tip.scrollIntoView({block:'center'});
				tip.click();
				const pop = tip.popoverTargetElement;
				checkLayout(pop && pop.matches(':popover-open'), 'help opens');
				if (!pop) continue;
				const box = rect(pop);
				checkLayout(box.left >= 0 && box.right <= document.documentElement.clientWidth + 1 && box.top >= 0 && box.bottom <= innerHeight + 1, 'help fits viewport');
				tip.click();
				checkLayout(!pop.matches(':popover-open'), 'help closes');
			}
			checkLayout(innerWidth === Number(location.search.slice(1)), 'viewport width');
			parent.document.body.dataset.layout=errors.join(',') || 'ok';
		})</script>`
		html := strings.Replace(buf.String(), "</body>", probe+"</body>", 1)
		mux := http.NewServeMux()
		mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(static))))
		mux.HandleFunc("/view", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, html) })
		// Chromium clamps top-level windows to a minimum width. An iframe gives
		// these probes the exact CSS viewport even on narrow phone screens.
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `<iframe width="%s" height="900" src="/view?%s"></iframe>`, url.QueryEscape(r.URL.RawQuery), url.QueryEscape(r.URL.RawQuery))
		})
		server := httptest.NewServer(mux)
		t.Cleanup(server.Close)
		for _, width := range []int{1280, 960, 390, 320} {
			t.Run(fmt.Sprintf("%s-%d", page.name, width), func(t *testing.T) {
				dom, err := exec.Command(browser, "--headless=new", "--no-sandbox", "--disable-gpu", "--user-data-dir="+t.TempDir(), "--dump-dom", "--window-size=1366,1000", fmt.Sprintf("%s/?%d", server.URL, width)).Output()
				if err != nil {
					t.Fatal(err)
				}
				_, result, _ := strings.Cut(string(dom), `data-layout="`)
				result, _, _ = strings.Cut(result, `"`)
				if result != "ok" {
					t.Fatalf("layout probe: %q", result)
				}
			})
		}
	}
}
