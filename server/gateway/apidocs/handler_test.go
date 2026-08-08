package apidocs

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandlerServesOfflineDocs(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		path string
		want string
	}{
		{path: "/docs/", want: "经典农场 API 文档"},
		{path: "/docs/http/", want: "swagger-ui"},
		{path: "/docs/ws/", want: "AsyncAPI"},
		{path: "/docs/grpc/", want: "SocialService"},
		{path: "/docs/openapi.yaml", want: "openapi: 3.1.0"},
		{path: "/docs/asyncapi.yaml", want: "asyncapi: 3.0.0"},
	} {
		test := test
		t.Run(test.path, func(t *testing.T) {
			t.Parallel()
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, test.path, nil)
			Handler().ServeHTTP(recorder, request)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", recorder.Code)
			}
			if !strings.Contains(recorder.Body.String(), test.want) {
				t.Fatalf("body does not contain %q", test.want)
			}
		})
	}
}
