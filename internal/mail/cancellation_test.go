package mail

import (
	"bufio"
	"context"
	"errors"
	"net"
	"strconv"
	"testing"
	"time"
)

func TestCancelingMailStopsAnUnresponsiveSMTPConversation(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			if _, err := conn.Write([]byte("220 fake ESMTP\r\n")); err == nil {
				if _, err := bufio.NewReader(conn).ReadString('\n'); err == nil {
					accepted <- conn
					return
				}
			}
			conn.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	p, _ := strconv.Atoi(port)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Send(ctx, Server{Host: "127.0.0.1", Port: p}, Message{From: "a@example.com", To: []string{"b@example.com"}})
	}()
	select {
	case conn := <-accepted:
		defer conn.Close()
	case <-time.After(2 * time.Second):
		t.Fatal("no SMTP connection")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation not preserved: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("mail kept waiting for the SMTP response after cancellation")
	}
}
