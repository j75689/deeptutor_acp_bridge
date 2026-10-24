package bridge

import (
	"context"
	"io"
	"net"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func TestParsePortRange(t *testing.T) {
	for in, want := range map[string][2]int{"38000-38031": {38000, 38031}, " 80 ": {80, 80}, "1-65535": {1, 65535}} {
		lo, hi, err := ParsePortRange(in)
		if err != nil || lo != want[0] || hi != want[1] {
			t.Errorf("ParsePortRange(%q) = %d,%d,%v", in, lo, hi, err)
		}
	}
	for _, in := range []string{"", "x", "10-5", "0-10", "1-70000", "1-x"} {
		if _, _, err := ParsePortRange(in); err == nil {
			t.Errorf("ParsePortRange(%q) should fail", in)
		}
	}
}

func firstHost(t *testing.T) string {
	t.Helper()
	hosts, _ := nonLoopbackIPv4()
	if len(hosts) == 0 {
		t.Skip("no non-loopback IPv4 address")
	}
	return hosts[0]
}

func dialEventually(t *testing.T, addr string, wantOK bool) net.Conn {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		c, err := net.DialTimeout("tcp4", addr, 300*time.Millisecond)
		if (err == nil) == wantOK {
			return c
		}
		if err == nil {
			c.Close()
		}
		if time.Now().After(deadline) {
			t.Fatalf("dial %s: wantOK=%v never happened (last err %v)", addr, wantOK, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// While the "agent" listens on 127.0.0.1:port, the relay serves the same port on
// the interface address; once the agent is gone the relay releases it.
func TestRelayFollowsAgentListener(t *testing.T) {
	host := firstHost(t)
	agent, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := agent.Addr().(*net.TCPAddr).Port
	go func() {
		for {
			c, err := agent.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				buf := make([]byte, 5)
				if _, err := io.ReadFull(c, buf); err == nil {
					_, _ = c.Write([]byte("echo:" + string(buf)))
				}
			}()
		}
	}()

	var present atomic.Bool
	r := &Relay{
		Lo: port, Hi: port, BindHosts: []string{host},
		Scan: func() ([]int, error) {
			if present.Load() {
				return []int{port}, nil
			}
			return nil, nil
		},
	}
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	addr := net.JoinHostPort(host, strconv.Itoa(port))

	// Nothing relayed until the agent is seen listening.
	dialEventually(t, addr, false)

	present.Store(true)
	c := dialEventually(t, addr, true)
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 10)
	if _, err := io.ReadFull(c, got); err != nil || string(got) != "echo:hello" {
		t.Fatalf("got %q, %v", got, err)
	}
	c.Close()

	present.Store(false)
	dialEventually(t, addr, false)
}

func TestProcLoopbackListenersFindsOwnListener(t *testing.T) {
	if _, err := procLoopbackListeners(1, 65535); err != nil {
		t.Skipf("no /proc/net/tcp: %v", err)
	}
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	ports, err := procLoopbackListeners(port, port)
	if err != nil || len(ports) != 1 || ports[0] != port {
		t.Fatalf("got %v, %v want [%d]", ports, err, port)
	}
}
