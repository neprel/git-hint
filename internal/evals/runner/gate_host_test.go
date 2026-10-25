package runner

import "testing"

func TestProxyAddressUsesTheCallerNamespace(t *testing.T) {
	for _, tc := range []struct{ caller, engineName, engineOS, endpoint, gateway, listen, host string }{
		{"darwin", "OrbStack", "linux", "unix:///docker.sock", "172.24.0.1", "127.0.0.1", "hint-proxy:host-gateway"},
		{"darwin", "Docker Desktop", "linux", "unix:///docker.sock", "172.24.0.1", "127.0.0.1", "hint-proxy:host-gateway"},
		{"linux", "Ubuntu", "linux", "unix:///docker.sock", "172.24.0.1", "172.24.0.1", "hint-proxy:172.24.0.1"},
		{"linux", "OrbStack", "linux", "unix:///docker.sock", "172.24.0.1", "172.24.0.1", "hint-proxy:172.24.0.1"},
		{"linux", "Docker Desktop", "linux", "unix:///docker.sock", "172.24.0.1", "172.24.0.1", "hint-proxy:172.24.0.1"},
	} {
		t.Run(tc.caller+"/"+tc.engineName, func(t *testing.T) {
			listen, host, e := proxyAddress(tc.caller, tc.engineName, tc.engineOS, tc.endpoint, tc.gateway)
			if e != nil || listen != tc.listen || host != tc.host {
				t.Fatalf("%q %q %v", listen, host, e)
			}
		})
	}
	for _, tc := range []struct{ caller, name, engine, endpoint, gateway string }{
		{"darwin", "Ubuntu", "linux", "tcp://remote:2376", "172.24.0.1"},
		{"linux", "Ubuntu", "linux", "tcp://remote:2376", "172.24.0.1"},
		{"linux", "Ubuntu", "linux", "unix:///docker.sock", ""},
		{"linux", "Ubuntu", "windows", "unix:///docker.sock", "172.24.0.1"},
		{"darwin", "Ubuntu", "linux", "unix:///docker.sock", "172.24.0.1"},
		{"freebsd", "Ubuntu", "linux", "unix:///docker.sock", "172.24.0.1"},
	} {
		if _, _, e := proxyAddress(tc.caller, tc.name, tc.engine, tc.endpoint, tc.gateway); e == nil {
			t.Fatalf("unsupported endpoint accepted: %+v", tc)
		}
	}
}
