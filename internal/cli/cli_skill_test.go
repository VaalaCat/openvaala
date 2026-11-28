package cli

import (
	"strings"
	"testing"
)

func TestSkillCommandArguments(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"skill", "list", "extra"}, "unknown command"},
		{[]string{"skill", "search"}, "accepts 1 arg"},
		{[]string{"skill", "get"}, "accepts between 1 and 2 arg"},
		{[]string{"skill", "get", "openvaala", "reference/cli.md", "extra"}, "accepts between 1 and 2 arg"},
	} {
		t.Run(strings.Join(tc.args[1:], " "), func(t *testing.T) {
			command := NewCLI(&CLIOpt{})
			command.SetArgs(tc.args)
			if err := command.ExecuteContext(t.Context()); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
}
