package link

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"crypto/tls"
	"crypto/x509"

	"github.com/danielpina1/athena/internal/pb/athenav1"
)

func TestDialAcceptRoundTrip(t *testing.T) {
	var got collector
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := Accept(w, r)
		if err != nil {
			return
		}
		_ = New(c, Options{OnFrame: got.add}).Run(r.Context())
	}))
	defer srv.Close()
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, err := Dial(ctx, "wss"+srv.URL[len("https"):], &tls.Config{RootCAs: pool})
	if err != nil {
		t.Fatal(err)
	}
	client := New(conn, Options{})
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx) }()
	if err := client.Send(athenav1.Channel_CONTROL, true, delta(1)); err != nil {
		t.Fatal(err)
	}
	if err := client.Send(athenav1.Channel_BLOB, true, delta(2)); err != nil {
		t.Fatal(err)
	}
	eventually(t, "both frames", func() bool { return got.len() == 2 })
	if c := got.cursors(); c[0] != 1 || c[1] != 2 {
		t.Fatalf("order %v", c)
	}
	eventually(t, "acked", func() bool { return len(client.Unacked()) == 0 })
	cancel()
	<-done
}

func TestDialRefusesUntrustedServer(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = Accept(w, r) }))
	srv.Config.ErrorLog = log.New(io.Discard, "", 0) // the expected failed handshake
	srv.StartTLS()
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := Dial(ctx, "wss"+srv.URL[len("https"):], &tls.Config{RootCAs: x509.NewCertPool()}); err == nil {
		t.Fatal("dial trusted an unknown certificate")
	}
}
