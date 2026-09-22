// Package envcompat resolves Nori environment variables while retaining the
// legacy Deploybot-prefixed names for backward compatibility.
package envcompat

import (
	"log"
	"os"
	"strings"
)

const (
	canonicalPrefix = "NORI_"
	legacyPrefix    = "DEPLOYBOT_"
)

// Get returns the canonical environment value, falling back to its legacy
// Deploybot-prefixed equivalent. A fallback emits a deprecation warning.
func Get(name string) string {
	value, legacy := Lookup(os.Getenv, name)
	if legacy {
		log.Printf("warning: %s is deprecated; use %s", LegacyName(name), name)
	}
	return value
}

// Lookup resolves name through get and reports whether the legacy fallback was
// used. It is useful for validating environment maps without logging.
func Lookup(get func(string) string, name string) (value string, legacy bool) {
	if value := get(name); value != "" {
		return value, false
	}
	if value := get(LegacyName(name)); value != "" {
		return value, true
	}
	return "", false
}

// LegacyName returns the deprecated Deploybot-prefixed equivalent of a Nori
// environment variable name.
func LegacyName(name string) string {
	return legacyPrefix + strings.TrimPrefix(name, canonicalPrefix)
}
