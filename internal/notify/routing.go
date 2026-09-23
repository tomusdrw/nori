package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"nori/internal/store"
)

// EventKind identifies the operational event a notification reports.
type EventKind string

const (
	// KindDeployFailed covers failed manual, automatic, and scheduled
	// deployments.
	KindDeployFailed EventKind = "deploy_failed"
	// KindDown covers outages and unhealthy services detected by the monitor.
	KindDown EventKind = "down"
	// KindRecovered is NotifyServiceRecovered.
	KindRecovered EventKind = "recovered"
	// KindSuccess is NotifyDeploySuccess.
	KindSuccess EventKind = "success"
)

// Channel identifiers used in the stored routing table and the settings form.
const (
	ChannelTwilio   = "twilio"
	ChannelTelegram = "telegram"
)

// Channels lists the canonical channels in settings-UI order.
var Channels = []string{ChannelTwilio, ChannelTelegram}

// Kinds lists the canonical event kinds in settings-UI order.
var Kinds = []EventKind{KindDeployFailed, KindDown, KindRecovered, KindSuccess}

// Route wraps one channel's notifier and consults Approve before every send,
// so per-channel event routing is enforced as close to the wire as possible.
// Approve is called at send time; a nil Approve forwards everything.
type Route struct {
	Channel string
	Approve func(EventKind) bool
	Inner   Notifier
}

func (r *Route) NotifyServiceDown(ctx context.Context, evt Event) error {
	kind := KindDeployFailed
	if evt.Trigger == store.TriggerMonitor {
		kind = KindDown
	}
	return r.send(ctx, kind, evt, r.Inner.NotifyServiceDown)
}

func (r *Route) NotifyServiceRecovered(ctx context.Context, evt Event) error {
	return r.send(ctx, KindRecovered, evt, r.Inner.NotifyServiceRecovered)
}

func (r *Route) NotifyDeploySuccess(ctx context.Context, evt Event) error {
	return r.send(ctx, KindSuccess, evt, r.Inner.NotifyDeploySuccess)
}

func (r *Route) send(ctx context.Context, kind EventKind, evt Event, deliver func(context.Context, Event) error) error {
	if r == nil || r.Inner == nil {
		return nil
	}
	if r.Approve != nil && !r.Approve(kind) {
		return nil
	}
	return deliver(ctx, evt)
}

// Routing maps each channel to the event kinds it should receive. A nil map
// behaves like a table that disables nothing explicitly.
type Routing map[string]map[EventKind]bool

// Allowed reports whether channel should receive kind. A channel absent from
// the table defaults to enabled for every kind, so a channel configured via
// environment after the table was saved starts with all events on. A channel
// present but missing a kind defaults to disabled — tables written by the
// settings form are always complete, so a gap means an explicit hand edit.
func (r Routing) Allowed(channel string, kind EventKind) bool {
	kinds, ok := r[channel]
	if !ok {
		return true
	}
	return kinds[kind]
}

// DefaultRouting enables every kind for every channel: the behavior of a
// fresh install before any routing choice was made.
func DefaultRouting() Routing {
	r := make(Routing, len(Channels))
	for _, ch := range Channels {
		kinds := make(map[EventKind]bool, len(Kinds))
		for _, kind := range Kinds {
			kinds[kind] = true
		}
		r[ch] = kinds
	}
	return r
}

// ParseRouting decodes the stored JSON table. Unknown channels and kinds are
// dropped so an older or newer schema degrades gracefully.
func ParseRouting(raw string) (Routing, error) {
	var r Routing
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return nil, fmt.Errorf("notify: parse routing: %w", err)
	}
	clean := make(Routing, len(r))
	for ch, kinds := range r {
		if !isChannel(ch) {
			continue
		}
		entry := make(map[EventKind]bool, len(kinds))
		for kind, on := range kinds {
			if isKind(kind) {
				entry[kind] = on
			}
		}
		// Tables saved before deploy failures and monitored outages became
		// separately configurable used "down" for both. Preserve that choice
		// until the administrator saves the expanded matrix.
		if _, ok := entry[KindDeployFailed]; !ok {
			if down, ok := entry[KindDown]; ok {
				entry[KindDeployFailed] = down
			}
		}
		clean[ch] = entry
	}
	return clean, nil
}

// RoutingFromLegacy translates a stored notify_mode value into an equivalent
// routing table. never disables everything; always, auto-only, and
// unrecognized values enable everything — auto-only's manual/auto trigger
// distinction has no per-event equivalent, so it approximates as all-on.
func RoutingFromLegacy(mode string) Routing {
	r := DefaultRouting()
	if mode == "never" {
		for _, ch := range Channels {
			for _, kind := range Kinds {
				r[ch][kind] = false
			}
		}
	}
	return r
}

// EffectiveRouting resolves the active table for a send: the stored
// notify_routing JSON when present, otherwise the translation of the legacy
// notify_mode value (which for an unset or unrecognized mode is the
// all-enabled default). A corrupt stored table logs and falls back to that
// same legacy translation, so a storage accident can never silence alerts
// beyond what the last known good choice implied. Call it at send time so
// settings changes apply without a restart.
func EffectiveRouting(routingJSON, legacyMode string) Routing {
	if routingJSON != "" {
		r, err := ParseRouting(routingJSON)
		if err == nil {
			return r
		}
		log.Printf("notify: ignoring corrupt routing table: %v", err)
	}
	if legacyMode == "" {
		return DefaultRouting()
	}
	return RoutingFromLegacy(legacyMode)
}

// MarshalRouting serializes a routing table for storage.
func MarshalRouting(r Routing) (string, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return "", fmt.Errorf("notify: encode routing: %w", err)
	}
	return string(b), nil
}

func isChannel(s string) bool {
	for _, ch := range Channels {
		if s == ch {
			return true
		}
	}
	return false
}

func isKind(k EventKind) bool {
	for _, kind := range Kinds {
		if k == kind {
			return true
		}
	}
	return false
}
