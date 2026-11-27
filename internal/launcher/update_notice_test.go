package launcher

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"nori/internal/notify"
)

type updateTestRunner struct {
	fakeRunner
	run func(args []string) error
}

func (f *updateTestRunner) Run(ctx context.Context, name string, args ...string) error {
	if err := f.fakeRunner.Run(ctx, name, args...); err != nil {
		return err
	}
	return f.run(args)
}

func TestNotifiedUpdateReportsOneOutcome(t *testing.T) {
	for _, tt := range []struct {
		name        string
		startError  error
		skipReport  bool
		checkResult string
		wantKind    notify.EventKind
		wantMessage string
	}{
		{name: "healthy", wantKind: notify.KindSuccess, wantMessage: "All monitored services are up"},
		{
			name: "service unhealthy", checkResult: "Nori restarted, but api and worker are still down",
			wantKind: notify.KindDeployFailed, wantMessage: "api and worker are still down",
		},
		{
			name: "cannot start Nori", startError: errors.New("port conflict"),
			wantKind: notify.KindDeployFailed, wantMessage: "port conflict",
		},
		{
			name: "replacement never reports", skipReport: true,
			wantKind: notify.KindDeployFailed, wantMessage: "did not report startup",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			l := &Launcher{ConfigDir: t.TempDir()}
			writeTestConfig(t, l, RunSpec{
				Image: "ghcr.io/acme/nori:latest", ContainerName: "nori", Restart: "unless-stopped",
				Volumes: []string{"/var/run/docker.sock:/var/run/docker.sock", "nori-config:/config"},
				Labels:  map[string]string{"nori.service": "nori"}, ConfigVolume: "nori-config", CurrentImageDigest: "sha256:old",
			})
			type notice struct {
				kinds   []notify.EventKind
				message string
			}
			var notices []notice
			runner := &updateTestRunner{run: func(args []string) error {
				switch args[0] {
				case "stop":
					if len(notices) != 1 || !strings.Contains(notices[0].message, "Nori is restarting") {
						t.Fatalf("expected one restart warning before stopping Nori, got %+v", notices)
					}
				case "run":
					if tt.startError != nil {
						return tt.startError
					}
					if tt.skipReport {
						cancel()
						return nil
					}
					for _, arg := range args {
						if token, ok := strings.CutPrefix(arg, "NORI_UPDATE_REPORT="); ok {
							t.Setenv("NORI_UPDATE_REPORT", token)
							return l.ReportUpdateStartup(ctx, func(context.Context) string { return tt.checkResult })
						}
					}
					t.Fatal("replacement did not receive a report token")
				}
				return nil
			}}
			l.Runner = runner

			err := l.update(ctx, "sha256:new", func(_ context.Context, msg string, kinds ...notify.EventKind) {
				notices = append(notices, notice{kinds, msg})
			})
			if (err == nil) != (tt.wantKind == notify.KindSuccess) {
				t.Fatalf("update error = %v", err)
			}
			if len(notices) != 2 || !slices.Contains(notices[1].kinds, tt.wantKind) || !strings.Contains(notices[1].message, tt.wantMessage) {
				t.Fatalf("expected warning and one %s outcome mentioning %q, got %+v", tt.wantKind, tt.wantMessage, notices)
			}
			if tt.checkResult != "" && !slices.Contains(notices[1].kinds, notify.KindDown) {
				t.Fatal("service-down subscribers should receive the failed service summary")
			}
			if tt.startError == nil && !tt.skipReport {
				if err := l.ReportUpdateStartup(ctx, func(context.Context) string {
					t.Fatal("startup check repeated after the launcher finished")
					return ""
				}); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestMutedChannelsSkipUpdateNotices(t *testing.T) {
	ctx := context.Background()
	runner := &fakeRunner{}
	l := &Launcher{ConfigDir: t.TempDir(), Runner: runner}
	writeTestConfig(t, l, RunSpec{
		Image: "ghcr.io/acme/nori:latest", ContainerName: "nori", Restart: "unless-stopped",
		Volumes: []string{"/var/run/docker.sock:/var/run/docker.sock", "nori-config:/config"},
		Labels:  map[string]string{"nori.service": "nori"}, ConfigVolume: "nori-config", CurrentImageDigest: "sha256:old",
	})
	if err := l.mergeEnvironment([]string{
		"NORI_TWILIO_ACCOUNT_SID=test", "NORI_TWILIO_AUTH_TOKEN=test", "NORI_TWILIO_FROM=+15555550101", "NORI_TWILIO_TO=+15555550102",
	}); err != nil {
		t.Fatal(err)
	}

	notice := notify.AutoUpdateNotice{Routing: notify.RoutingFromLegacy("never")}
	if err := l.UpdateWithNotice(ctx, "sha256:new", notice); err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(runner.calls[len(runner.calls)-1], func(arg string) bool {
		return strings.HasPrefix(arg, "NORI_UPDATE_REPORT=")
	}) {
		t.Fatal("muted channels should not wait for an update report")
	}
}
