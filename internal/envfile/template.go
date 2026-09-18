package envfile

import (
	"errors"
	"sort"
	"strings"
)

const Placeholder = "[REDACTED]"
const MaxSize = 128 * 1024

// Template exposes only variable names. Comments are omitted because they may
// contain credentials too. Every value is treated as secret, including blanks.
func Template(content string) (string, error) {
	values, err := templateValues(content)
	if err != nil {
		return "", err
	}
	for key := range values {
		values[key] = Placeholder
	}
	return marshalValues(values), nil
}

// ResolveTemplate preserves values by name, removes omitted keys, and gives new
// keys an empty value. Actual values can only be supplied through SetValue.
func ResolveTemplate(current, template string) (string, error) {
	keys, err := templateValues(template)
	if err != nil {
		return "", err
	}
	values, err := templateValues(current)
	if err != nil {
		return "", err
	}
	for key, value := range keys {
		if value != Placeholder {
			return "", errors.New("environment values must be [REDACTED] placeholders; use set_service_secret to set values")
		}
		keys[key] = values[key]
	}
	return boundedValues(keys)
}

// SetValue only replaces a key already declared in the configuration.
func SetValue(current, key, value string) (string, error) {
	if !validName.MatchString(key) {
		return "", errors.New("invalid environment variable name")
	}
	values, err := templateValues(current)
	if err != nil {
		return "", err
	}
	if _, ok := values[key]; !ok {
		return "", errors.New("environment variable must already exist in the configuration")
	}
	if strings.ContainsRune(value, 0) {
		return "", errors.New("environment value must not contain NUL")
	}
	values[key] = value
	return boundedValues(values)
}

func templateValues(content string) (map[string]string, error) {
	if len(content) > MaxSize {
		return nil, errors.New("environment must be at most 128 KiB")
	}
	entries, err := Parse(content)
	if err != nil {
		return nil, errors.New("invalid dotenv syntax")
	}
	values := make(map[string]string, len(entries))
	for _, entry := range entries {
		key, value, _ := strings.Cut(entry, "=")
		values[key] = value
	}
	return values, nil
}

func boundedValues(values map[string]string) (string, error) {
	content := marshalValues(values)
	if len(content) > MaxSize {
		return "", errors.New("environment must be at most 128 KiB")
	}
	// The parser is the runtime contract: never silently corrupt an inserted value.
	parsed, err := templateValues(content)
	if err != nil {
		return "", errors.New("environment value cannot be represented as dotenv")
	}
	for key, value := range values {
		if parsed[key] != value {
			return "", errors.New("environment value cannot be represented as dotenv")
		}
	}
	return content, nil
}

func marshalValues(values map[string]string) string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	// Always quote values: numeric-looking credentials must retain leading zeros.
	escape := strings.NewReplacer("\\", "\\\\", "\n", "\\n", "\r", "\\r", "\"", "\\\"", "$", "\\$")
	var out strings.Builder
	for _, key := range keys {
		out.WriteString(key + "=\"" + escape.Replace(values[key]) + "\"\n")
	}
	return out.String()
}
