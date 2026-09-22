package notify

import (
	"context"
	"strings"
	"testing"
)

func TestRoute_ForwardsApprovedKinds(t *testing.T) {
	inner := &recordingNotifier{name: "inner"}
	r := &Route{
		Channel: ChannelTwilio,
		Approve: func(EventKind) bool { return true },
		Inner:   inner,
	}
	ctx := context.Background()
	evt := Event{ServiceName: "app"}
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
	_ = r.NotifyServiceDown(ctx, Event{})
	_ = r.NotifyServiceRecovered(ctx, Event{})
	_ = r.NotifyDeploySuccess(ctx, Event{})
	if want := "down"; strings.Join(inner.calls, ",") != want {
		t.Errorf("inner calls = %v, want only %v", inner.calls, want)
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
