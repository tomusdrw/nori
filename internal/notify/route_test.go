package notify

import (
	"context"
	"strings"
	"testing"

	"nori/internal/store"
)

func TestRoute_ForwardsApprovedKinds(t *testing.T) {
	inner := &recordingNotifier{name: "inner"}
	r := &Route{
		Channel: ChannelTwilio,
		Approve: func(EventKind) bool { return true },
		Inner:   inner,
	}
	ctx := context.Background()
	evt := Event{ServiceName: "app", Trigger: store.TriggerManual}
	if err := r.NotifyServiceDown(ctx, evt); err != nil {
		t.Fatalf("NotifyServiceDown: %v", err)
	}
	if err := r.NotifyServiceRecovered(ctx, evt); err != nil {
		t.Fatalf("NotifyServiceRecovered: %v", err)
	}
	if err := r.NotifyDeploySuccess(ctx, evt); err != nil {
		t.Fatalf("NotifyDeploySuccess: %v", err)
	}
	if want := "down,recovered,success"; strings.Join(inner.calls, ",") != want {
		t.Errorf("inner calls = %v, want %v", inner.calls, want)
	}
}

func TestRoute_BlocksUnapprovedKinds(t *testing.T) {
	inner := &recordingNotifier{name: "inner"}
	r := &Route{
		Channel: ChannelTwilio,
		Approve: func(kind EventKind) bool { return kind == KindDown },
		Inner:   inner,
	}
	ctx := context.Background()
	_ = r.NotifyServiceDown(ctx, Event{Trigger: store.TriggerMonitor})
	_ = r.NotifyServiceRecovered(ctx, Event{})
	_ = r.NotifyDeploySuccess(ctx, Event{})
	if want := "down"; strings.Join(inner.calls, ",") != want {
		t.Errorf("inner calls = %v, want only %v", inner.calls, want)
	}
}

func TestRoute_RoutesDeployFailuresSeparatelyFromMonitoredOutages(t *testing.T) {
	inner := &recordingNotifier{name: "inner"}
	var seen []EventKind
	r := &Route{
		Channel: ChannelTwilio,
		Approve: func(kind EventKind) bool {
			seen = append(seen, kind)
			return kind == KindDeployFailed
		},
		Inner: inner,
	}

	_ = r.NotifyServiceDown(context.Background(), Event{Trigger: store.TriggerManual})
	_ = r.NotifyServiceDown(context.Background(), Event{Trigger: store.TriggerMonitor})

	if got, want := strings.Join(inner.calls, ","), "down"; got != want {
		t.Fatalf("inner calls = %q, want %q", got, want)
	}
	if len(seen) != 2 || seen[0] != KindDeployFailed || seen[1] != KindDown {
		t.Fatalf("approved kinds = %v, want [%s %s]", seen, KindDeployFailed, KindDown)
	}
}

func TestRoute_NilApproveForwardsEverything(t *testing.T) {
	inner := &recordingNotifier{name: "inner"}
	r := &Route{Channel: ChannelTelegram, Inner: inner}
	ctx := context.Background()
	_ = r.NotifyServiceDown(ctx, Event{})
	_ = r.NotifyServiceRecovered(ctx, Event{})
	_ = r.NotifyDeploySuccess(ctx, Event{})
	if want := "down,recovered,success"; strings.Join(inner.calls, ",") != want {
		t.Errorf("inner calls = %v, want %v", inner.calls, want)
	}
}

func TestRoute_ErrorsPropagate(t *testing.T) {
	wantErr := errTest
	r := &Route{
		Channel: ChannelTwilio,
		Approve: func(EventKind) bool { return true },
		Inner:   &recordingNotifier{name: "inner", err: wantErr},
	}
	if err := r.NotifyServiceDown(context.Background(), Event{}); err != wantErr {
		t.Errorf("err = %v, want %v", err, wantErr)
	}
}

var errTest = errorString("boom")

type errorString string

func (e errorString) Error() string { return string(e) }
