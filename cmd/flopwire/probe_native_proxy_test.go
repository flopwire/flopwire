package main

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

func TestNativeWakeProxyFramesJSONLOverDaemonWebSocket(t *testing.T) {
	path := filepath.Join(shortSockDir(t), "daemon.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: websocket.Handler(func(ws *websocket.Conn) {
		defer ws.Close()
		var request string
		if err := websocket.Message.Receive(ws, &request); err != nil {
			return
		}
		if request != `{"id":1,"method":"initialize"}` {
			t.Errorf("wrong request frame: %q", request)
		}
		websocket.Message.Send(ws, `{"id":1,"result":{}}`)
		var end string
		websocket.Message.Receive(ws, &end)
	})}
	go server.Serve(listener)
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	input, in := io.Pipe()
	out, output := io.Pipe()
	defer input.Close()
	defer in.Close()
	defer out.Close()
	defer output.Close()
	done := make(chan error, 1)
	go func() { done <- runNativeProxy(ctx, path, input, output) }()
	if _, err := io.WriteString(in, "{\"id\":1,\"method\":\"initialize\"}\n"); err != nil {
		t.Fatal(err)
	}
	response := make(chan string, 1)
	go func() { line, _ := bufio.NewReader(out).ReadString('\n'); response <- line }()
	select {
	case line := <-response:
		if line != "{\"id\":1,\"result\":{}}\n" {
			t.Fatalf("wrong response: %q", line)
		}
	case <-ctx.Done():
		t.Fatal("WebSocket response did not reach JSONL driver")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("proxy did not stop after cancellation")
	}
}
