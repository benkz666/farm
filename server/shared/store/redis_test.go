package store

import "testing"

func TestSessionValidationStatus(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		value any
		want  int64
	}{
		{name: "integer response", value: int64(1), want: 1},
		{name: "string response", value: "2", want: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := sessionValidationStatus(test.value)
			if err != nil {
				t.Fatalf("sessionValidationStatus: %v", err)
			}
			if got != test.want {
				t.Fatalf("status = %d, want %d", got, test.want)
			}
		})
	}
}

func TestSessionValidationStatusRejectsMalformedResponse(t *testing.T) {
	t.Parallel()
	if _, err := sessionValidationStatus([]byte("1")); err == nil {
		t.Fatal("sessionValidationStatus unexpectedly accepted []byte")
	}
}
