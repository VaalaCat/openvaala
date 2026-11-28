package world

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"slices"
	"testing"
)

func TestSelectRelayInstances(t *testing.T) {
	data, err := os.ReadFile("testdata/relay-hrw.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name, Key           string
		Instances, Selected []string
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			key, err := base64.RawURLEncoding.DecodeString(tc.Key)
			if err != nil {
				t.Fatal(err)
			}
			if got := selectRelayInstances(tc.Instances, key); !slices.Equal(got, tc.Selected) {
				t.Fatalf("selected %v, want %v", got, tc.Selected)
			}
		})
	}
	if !slices.Contains(cases[3].Selected, cases[2].Selected[1]) {
		t.Fatal("removing the first instance must retain the second choice")
	}
}
