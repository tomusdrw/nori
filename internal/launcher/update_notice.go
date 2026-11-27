package launcher

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"nori/internal/config"
	"nori/internal/notify"
)

const (
	updateReportEnv  = "NORI_UPDATE_REPORT"
	updateReportFile = "startup"
	updateCheckTime  = time.Minute
	updateReportWait = 2 * time.Minute
)

var updateReportName = regexp.MustCompile(`^update-[0-9]+$`)

type updateNoticeSender func(context.Context, string, ...notify.EventKind)

// UpdateWithNotice reports an automatic update through the configured
// channels, including when the replacement cannot start. The detached
// launcher loads credentials from the same env file it uses to start Nori.
func (l *Launcher) UpdateWithNotice(ctx context.Context, digest string, notice notify.AutoUpdateNotice) error {
	env, err := l.loadEnvironment()
	if err != nil {
		log.Printf("self-update notifications: %v", err)
		return l.Update(ctx, digest)
	}
	twilio, telegram, err := config.LoadNotificationChannels(func(key string) string { return env[key] })
	if err != nil {
		log.Printf("self-update notifications: %v", err)
		return l.Update(ctx, digest)
	}

	channels := make(map[string]func(context.Context, string) error)
	wantsUpdate := func(channel string) bool {
		return notice.Routing.Allowed(channel, notify.KindDown) ||
			notice.Routing.Allowed(channel, notify.KindSuccess) ||
			notice.Routing.Allowed(channel, notify.KindDeployFailed)
	}
	if twilio.Enabled() && wantsUpdate(notify.ChannelTwilio) {
		channels[notify.ChannelTwilio] = notify.NewTwilio(twilio.AccountSID, twilio.AuthToken, twilio.From, twilio.To).SendAdmin
	}
	if telegram.Enabled() && wantsUpdate(notify.ChannelTelegram) {
		channels[notify.ChannelTelegram] = notify.NewTelegram(telegram.BotToken, telegram.ChatID).SendAdmin
	}
	if len(channels) == 0 {
		return l.Update(ctx, digest)
	}

	return l.update(ctx, digest, func(ctx context.Context, message string, kinds ...notify.EventKind) {
		if notice.BotName != "" && notice.BotName != "Nori" {
			message = notice.BotName + " — " + message
		}
		const maxRunes = 500
		if runes := []rune(message); len(runes) > maxRunes {
			message = string(runes[:maxRunes-1]) + "…"
		}
		for channel, send := range channels {
			if slices.ContainsFunc(kinds, func(kind notify.EventKind) bool { return notice.Routing.Allowed(channel, kind) }) {
				if err := send(ctx, message); err != nil {
					log.Printf("self-update notification via %s: %v", channel, err)
				}
			}
		}
	})
}

func sendUpdateNotice(ctx context.Context, send updateNoticeSender, message string, kinds ...notify.EventKind) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 22*time.Second)
	defer cancel()
	send(ctx, message, kinds...)
}

func waitForUpdateReport(ctx context.Context, dir string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, updateReportWait)
	defer cancel()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		data, err := os.ReadFile(filepath.Join(dir, updateReportFile))
		if err == nil {
			return string(data), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("read startup report: %w", err)
		}
		select {
		case <-ctx.Done():
			return "", errors.New("the replacement did not report startup and service health in time")
		case <-tick.C:
		}
	}
}

// ReportUpdateStartup runs the startup health check only while a launcher is
// waiting for this replacement. An empty check result reports success.
func (l *Launcher) ReportUpdateStartup(ctx context.Context, check func(context.Context) string) error {
	name := os.Getenv(updateReportEnv)
	if !updateReportName.MatchString(name) {
		return nil
	}
	path := filepath.Join(l.configDir(), name, updateReportFile)
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if _, err := os.Stat(filepath.Dir(path)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	checkCtx, cancel := context.WithTimeout(ctx, updateCheckTime)
	defer cancel()
	result := check(checkCtx)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return writeAtomicInExistingDir(path, []byte(strings.TrimSpace(result)), 0o600)
}
