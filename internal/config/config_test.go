// Covers: internal/config (D-024 admission default, D-025 autocreate default, D-031 cluster token)
package config

import (
	"strings"
	"testing"
)

func lookup(env map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := env[k]; return v, ok }
}

func TestTenancyDefaultsByEnvironment(t *testing.T) {
	cases := []struct {
		env        map[string]string
		admission  string
		autocreate bool
	}{
		{map[string]string{}, "password", true}, // development: the v1 cut-over shape
		{map[string]string{"REDLITE_ENVIRONMENT": "staging"}, "password", false},
		{map[string]string{"REDLITE_ENVIRONMENT": "production", "REDLITE_CLUSTER_TOKEN": "t"}, "key", false},
		// criterion 2: autocreate set explicitly is still ignored outside development
		{map[string]string{"REDLITE_ENVIRONMENT": "production", "REDLITE_CLUSTER_TOKEN": "t", "REDLITE_SHEET_AUTOCREATE": "true"}, "key", false},
		// criterion 12: the advomedix cut-over shape in production is expressible
		{map[string]string{"REDLITE_ENVIRONMENT": "production", "REDLITE_CLUSTER_TOKEN": "t", "REDLITE_ADMISSION": "password"}, "password", false},
		{map[string]string{"REDLITE_SHEET_AUTOCREATE": "false"}, "password", false},
	}
	for _, c := range cases {
		cfg, err := build(lookup(c.env))
		if err != nil {
			t.Fatalf("%v: %v", c.env, err)
		}
		if cfg.Admission != c.admission || cfg.SheetAutocreate != c.autocreate {
			t.Errorf("%v: admission=%s autocreate=%v, want %s %v", c.env, cfg.Admission, cfg.SheetAutocreate, c.admission, c.autocreate)
		}
	}
}

// TestProductionNeedsClusterToken is criterion 10's config half: Load fails, naming the variable,
// so cmd/redlite exits 1 with its one Error line.
func TestProductionNeedsClusterToken(t *testing.T) {
	_, err := build(lookup(map[string]string{"REDLITE_ENVIRONMENT": "production"}))
	if err == nil || !strings.Contains(err.Error(), "REDLITE_CLUSTER_TOKEN") {
		t.Fatalf("production without a cluster token: err = %v", err)
	}
	if _, err := build(lookup(map[string]string{"REDLITE_ADMISSION": "open"})); err == nil {
		t.Fatal("REDLITE_ADMISSION=open accepted")
	}
}
