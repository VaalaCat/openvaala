package cli

import (
	"strings"
	"testing"
)

func TestExecCommandArguments(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"requires separator", []string{"exec", "--executor", "local", "pwd"}, "exec requires a command after --"},
		{"requires executor", []string{"exec", "--", "pwd"}, "required flag(s)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			command := NewCLI(&CLIOpt{})
			command.SetArgs(tc.args)
			if err := command.ExecuteContext(t.Context()); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
}
