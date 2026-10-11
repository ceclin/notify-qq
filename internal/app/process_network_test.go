package app_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// The real command keeps its production URLs. A local CONNECT proxy and a
// certificate for that host exercise ordinary HTTP/TLS without live QQ access.
func newProcessQQServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		DNSNames:    []string{"api.bot.qq.com"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		KeyUsage:    x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	certificate, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{certificate}, PrivateKey: key}}}
	server.StartTLS()
	return server
}

func newProcessQQProxy(t *testing.T, upstream *httptest.Server) *httptest.Server {
	t.Helper()
	var handlers sync.WaitGroup
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		handlers.Add(1)
		defer handlers.Done()
		if request.Method != http.MethodConnect || request.Host != "api.bot.qq.com:443" {
			t.Errorf("process attempted unexpected external destination: %s %s", request.Method, request.Host)
			http.Error(w, "unexpected destination", http.StatusBadGateway)
			return
		}
		target, err := (&net.Dialer{Timeout: time.Second}).DialContext(request.Context(), "tcp", upstream.Listener.Addr().String())
		if err != nil {
			t.Error(err)
			http.Error(w, "local upstream unavailable", http.StatusBadGateway)
			return
		}
		defer func() { _ = target.Close() }()
		client, buffered, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = client.Close() }()
		deadline := time.Now().Add(15 * time.Second)
		if err := client.SetDeadline(deadline); err != nil {
			t.Error(err)
			return
		}
		if err := target.SetDeadline(deadline); err != nil {
			t.Error(err)
			return
		}
		if _, err := buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
			t.Error(err)
			return
		}
		if err := buffered.Flush(); err != nil {
			t.Error(err)
			return
		}
		// Either peer closing releases both directions. The handler joins its
		// worker; copy/close failures during process exit are expected cleanup.
		done := make(chan struct{})
		go func() {
			_, _ = io.Copy(target, buffered)
			_ = target.Close()
			close(done)
		}()
		_, _ = io.Copy(client, target)
		_ = client.Close()
		_ = target.Close()
		<-done
	}))
	t.Cleanup(func() {
		proxy.Close()
		// Hijacked connections leave net/http's active-connection tracking.
		handlers.Wait()
	})
	return proxy
}

func processQQCertificate(upstream *httptest.Server) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: upstream.Certificate().Raw})
}
