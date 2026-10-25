package runner

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/moby/moby/client"
)

type dockerConnectionFailure struct{ err error }

func (r dockerConnectionFailure) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, r.err
}

func TestDockerConnectionPermissionIsDistinctWithoutExposingTheEndpoint(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"permission", &net.OpError{Op: "dial", Net: "unix", Err: &os.PathError{Op: "connect", Path: "/private/synthetic-engine.sock", Err: fs.ErrPermission}}, "permission denied connecting to the Docker engine"},
		{"unreachable", errors.New("synthetic transport failure"), "the Docker engine is not running or not reachable"},
		{"unrelated permission text", errors.New("permission denied from an unrelated transport failure"), "the Docker engine is not running or not reachable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, err := findDockerWithOptions(context.Background(), client.WithHost("unix:///private/synthetic-engine.sock"), client.WithHTTPClient(&http.Client{Transport: dockerConnectionFailure{tc.err}}))
			if d != nil || !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("unexpected connection result: %v", err)
			}
			if strings.Contains(err.Error(), "/private/") || strings.Contains(err.Error(), "synthetic-engine") {
				t.Fatal("connection error exposed the local endpoint")
			}
		})
	}
}
