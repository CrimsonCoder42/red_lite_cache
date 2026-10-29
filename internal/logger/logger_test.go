// Covers: internal/logger
package logger

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestMaskHidesSecretsAndValues(t *testing.T) {
	for _, jsonOut := range []bool{false, true} {
		var buf bytes.Buffer
		l := New(&buf, slog.LevelDebug, jsonOut)
		l.Info("Sheet: test", "sheet", "s1", "password", "hunter2", "value", `{"ssn":"123"}`,
			"key_hash", "abc", "database_url", "postgres://u:p@h/db", "password_hash", "$2a$xx", "api_key", "rlk_secret")
		out := buf.String()
		for _, leaked := range []string{"hunter2", "123", "abc", "postgres://", "$2a$xx", "rlk_secret"} {
			if strings.Contains(out, leaked) {
				t.Fatalf("json=%v: log line leaked %q: %s", jsonOut, leaked, out)
			}
		}
		if !strings.Contains(out, "s1") {
			t.Fatalf("json=%v: identifier was masked: %s", jsonOut, out)
		}
	}
}
