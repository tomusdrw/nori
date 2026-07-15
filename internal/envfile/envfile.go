package envfile

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/joho/godotenv"
)

var validName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Parse validates a dotenv document and returns a deterministic process
// environment. Blank files and comments are valid.
func Parse(content string) ([]string, error) {
	if strings.TrimSpace(content) == "" {
		return nil, nil
	}

	values, err := godotenv.Unmarshal(content)
	if err != nil {
		return nil, fmt.Errorf("invalid dotenv syntax: %w", err)
	}

	keys := make([]string, 0, len(values))
	for key := range values {
		if !validName.MatchString(key) {
			return nil, fmt.Errorf("invalid environment variable name %q", key)
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)

	env := make([]string, 0, len(keys))
	for _, key := range keys {
		env = append(env, key+"="+values[key])
	}
	return env, nil
}
