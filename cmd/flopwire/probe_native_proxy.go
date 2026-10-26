package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"golang.org/x/net/websocket"
)

// The daemon's control socket speaks WebSocket, whereas the existing probe
// drivers speak JSONL. Keep this adapter private to the native-wake probe.
func probeNativeProxy(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("native-proxy", flag.ContinueOnError)
	path := fs.String("socket", "", "scratch daemon socket")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *path == "" || fs.NArg() != 0 {
		return errors.New("native-proxy requires --socket")
	}
	return runNativeProxy(ctx, *path, os.Stdin, os.Stdout)
}

func runNativeProxy(ctx context.Context, path string, input io.Reader, output io.Writer) error {
	conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", path)
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	cfg, err := websocket.NewConfig("ws://localhost/", "http://localhost/")
	if err != nil {
		return err
	}
	ws, err := websocket.NewClient(cfg, conn)
	if err != nil {
		return err
	}
	defer ws.Close()
	conn.SetDeadline(time.Time{})
	finished := make(chan struct{})
	defer close(finished)
	go func() {
		select {
		case <-ctx.Done():
			ws.Close()
		case <-finished:
		}
	}()
	go func() {
		scan := bufio.NewScanner(input)
		scan.Buffer(make([]byte, 4096), 4<<20)
		for scan.Scan() {
			if err := websocket.Message.Send(ws, scan.Text()); err != nil {
				break
			}
		}
		ws.Close()
	}()
	for {
		var message string
		if err := websocket.Message.Receive(ws, &message); err != nil {
			if errors.Is(err, io.EOF) || ctx.Err() != nil {
				return nil
			}
			return err
		}
		if _, err := fmt.Fprintln(output, message); err != nil {
			return err
		}
	}
}
