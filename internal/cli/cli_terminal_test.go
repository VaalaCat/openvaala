package cli

import (
	"strings"
	"testing"
)

func TestTerminalCommandArguments(t *testing.T) {
	for _, tc := range []struct {
		name string
		want string
	}{
		{"close", "accepts 1 arg"},
		{"send", "accepts 2 arg"},
		{"capture", "accepts 1 arg"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			command := NewCLI(&CLIOpt{})
			command.SetArgs([]string{"terminal", tc.name})
			if err := command.ExecuteContext(t.Context()); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
}
