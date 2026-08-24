package proxy

import (
	"bufio"
	"context"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestHTTPConnectUsesProxyAuthorization(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	headerCh := make(chan http.Header, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		req, err := http.ReadRequest(bufio.NewReader(conn))
		if err != nil {
			return
		}
		headerCh <- req.Header.Clone()
		_, _ = io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
		_, _ = io.Copy(conn, conn)
	}()
	host, port, _ := net.SplitHostPort(listener.Addr().String())
	c := Config{Type: KindHTTP, Host: host, Port: port, User: "alice", Pass: "secret"}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := c.tunnelDialContext(ctx, "tcp", "example.com:443")
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	headers := <-headerCh
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:secret"))
	if headers.Get("Proxy-Authorization") != want {
		t.Fatalf("Proxy-Authorization=%q", headers.Get("Proxy-Authorization"))
	}
	if headers.Get("Authorization") != "" {
		t.Fatal("proxy credentials leaked into origin Authorization header")
	}
}

func TestSOCKS4RejectedExplicitly(t *testing.T) {
	_, err := Parse("socks4://127.0.0.1:1080")
	if err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("expected explicit SOCKS4 rejection, got %v", err)
	}
}

func TestSOCKSContextCancellation(t *testing.T) {
	c := Config{Type: KindSOCKS5, Host: "192.0.2.1", Port: "1080"}
	dial, err := c.socksDialContext()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	_, err = dial(ctx, "tcp", "example.com:443")
	if err == nil {
		t.Fatal("expected canceled dial")
	}
	if time.Since(start) > time.Second {
		t.Fatalf("canceled dial returned too slowly: %v", time.Since(start))
	}
}
