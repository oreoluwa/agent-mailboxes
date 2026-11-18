package imapingest

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

func TestValidateCredentialsContextTimesOutOnHangingGreeting(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	acceptedDone := make(chan struct{})
	go func() {
		defer close(acceptedDone)
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		buffer := make([]byte, 1)
		_, _ = conn.Read(buffer)
	}()
	host, port := splitTestAddress(t, listener.Addr().String())
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()
	err = (IMAPFetcher{Host: host, Port: port, Username: "user", Password: "secret"}).ValidateCredentialsContext(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error=%v, want deadline exceeded", err)
	}
	select {
	case <-acceptedDone:
	case <-time.After(time.Second):
		t.Fatal("credential validation left the hanging connection open")
	}
}

func TestValidateCredentialsContextCancellationInterruptsLogin(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	loginStarted := make(chan struct{})
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		_, _ = conn.Write([]byte("* OK synthetic IMAP ready\r\n"))
		buffer := make([]byte, 1024)
		if _, readErr := conn.Read(buffer); readErr == nil {
			close(loginStarted)
		}
		_, _ = conn.Read(buffer)
	}()
	host, port := splitTestAddress(t, listener.Addr().String())
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- (IMAPFetcher{Host: host, Port: port, Username: "user", Password: "secret"}).ValidateCredentialsContext(ctx)
	}()
	select {
	case <-loginStarted:
		cancel()
	case <-time.After(time.Second):
		t.Fatal("client did not reach login")
	}
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v, want context canceled", err)
	}
	select {
	case <-serverDone:
	case <-time.After(time.Second):
		t.Fatal("canceled validation left login connection open")
	}
}

func splitTestAddress(t *testing.T, address string) (string, int) {
	t.Helper()
	host, rawPort, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	port, err := net.LookupPort("tcp", rawPort)
	if err != nil {
		t.Fatal(err)
	}
	return host, port
}
