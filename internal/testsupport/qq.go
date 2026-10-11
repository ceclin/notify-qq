// Package testsupport keeps QQ behavior fixtures local while production uses
// the documented API addresses. Production packages must not import it.
package testsupport

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func QQClient(t testing.TB, server *httptest.Server) *http.Client {
	t.Helper()
	destination, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := server.Client()
	client.Transport = qqTransport{next: client.Transport, destination: destination}
	return client
}

type qqTransport struct {
	next        http.RoundTripper
	destination *url.URL
}

func (transport qqTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Scheme == "https" && request.URL.Host == "api.bot.qq.com" {
		request = request.Clone(request.Context())
		request.URL.Scheme, request.URL.Host = transport.destination.Scheme, transport.destination.Host
		request.Host = transport.destination.Host
	} else if address := net.ParseIP(request.URL.Hostname()); !address.IsLoopback() {
		return nil, fmt.Errorf("QQ fixture rejected nonlocal destination %q", request.URL.Host)
	}
	return transport.next.RoundTrip(request)
}
