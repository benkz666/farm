package obs_test

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"farm/server/platform/obs"
)

func TestLoggerOmitsSecretsAndUsesStableFields(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := obs.NewLogger(&buf, slog.LevelInfo)
	logger.Error("actor flush failed",
		"component", "actor",
		"op", "flush",
		"err", "disk full",
	)
	out := buf.String()
	for _, want := range []string{`"msg":"actor flush failed"`, `"component":"actor"`, `"op":"flush"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("log missing %s in %s", want, out)
		}
	}
	for _, forbidden := range []string{"token", "secret", "Bearer ", "password"} {
		if strings.Contains(strings.ToLower(out), forbidden) {
			t.Fatalf("log leaked sensitive key %q: %s", forbidden, out)
		}
	}
}
