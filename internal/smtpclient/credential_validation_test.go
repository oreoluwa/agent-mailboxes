package smtpclient

import (
	"context"
	"errors"
	"net"
	"strconv"
	"testing"
	"time"
)

func TestValidateCredentialsTimesOutOnHangingGreeting(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		buffer := make([]byte, 1)
		_, _ = conn.Read(buffer)
	}()
	host, rawPort, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()
	err = (Client{Host: host, Port: port, Username: "user", Password: "secret"}).ValidateCredentials(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error=%v, want deadline exceeded", err)
	}
	select {
	case <-serverDone:
	case <-time.After(time.Second):
		t.Fatal("credential validation left the hanging SMTP connection open")
	}
}
