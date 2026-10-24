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
	r.SetActive(true)
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

// startEchoAgent listens on 127.0.0.1:0 like the agent's callback server and
// answers every connection with "echo:"+5 bytes.
func startEchoAgent(t *testing.T) int {
	t.Helper()
	agent, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { agent.Close() })
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
	return agent.Addr().(*net.TCPAddr).Port
}

// Loopback services the agent opens after sign-in must stay unreachable: the
// relay does nothing until activated and stops again when deactivated.
func TestRelayOnlyWhileActive(t *testing.T) {
	host := firstHost(t)
	port := startEchoAgent(t)
	r := &Relay{
		Lo: port, Hi: port, BindHosts: []string{host},
		Scan: func() ([]int, error) { return []int{port}, nil },
	}
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	addr := net.JoinHostPort(host, strconv.Itoa(port))

	time.Sleep(3 * relayPollEvery) // the agent is listening, but the relay is off
	dialEventually(t, addr, false)

	r.SetActive(true)
	dialEventually(t, addr, true).Close()

	r.SetActive(false)
	dialEventually(t, addr, false)
}

// A callback is a single request: after one relayed connection the port is
// released and not re-opened for the rest of the window, even though the agent
// is still listening on it.
func TestRelayPortIsSingleUse(t *testing.T) {
	host := firstHost(t)
	port := startEchoAgent(t)
	r := &Relay{
		Lo: port, Hi: port, BindHosts: []string{host},
		Scan: func() ([]int, error) { return []int{port}, nil },
	}
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	r.SetActive(true)
	addr := net.JoinHostPort(host, strconv.Itoa(port))

	c := dialEventually(t, addr, true)
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	_, _ = c.Write([]byte("hello"))
	got := make([]byte, 10)
	if _, err := io.ReadFull(c, got); err != nil || string(got) != "echo:hello" {
		t.Fatalf("got %q, %v", got, err)
	}
	c.Close()

	time.Sleep(3 * relayPollEvery)
	dialEventually(t, addr, false)

	// A new sign-in window starts fresh.
	r.SetActive(false)
	r.SetActive(true)
	dialEventually(t, addr, true).Close()
}

// The agent's callback server handles one connection at a time. An idle
// (speculative) connection to the relay must not occupy it, or the real request
// queues behind it forever.
func TestRelayIdleConnectionDoesNotBlockAgent(t *testing.T) {
	host := firstHost(t)
	agent, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { agent.Close() })
	port := agent.Addr().(*net.TCPAddr).Port
	go func() { // serial, like wsgiref's handle_request loop
		for {
			c, err := agent.Accept()
			if err != nil {
				return
			}
			buf := make([]byte, 5)
			if _, err := io.ReadFull(c, buf); err == nil {
				_, _ = c.Write([]byte("echo:" + string(buf)))
			}
			c.Close()
		}
	}()

	r := &Relay{
		Lo: port, Hi: port, BindHosts: []string{host},
		Scan: func() ([]int, error) { return []int{port}, nil },
	}
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	r.SetActive(true)
	addr := net.JoinHostPort(host, strconv.Itoa(port))

	idle := dialEventually(t, addr, true) // never sends anything
	defer idle.Close()
	time.Sleep(300 * time.Millisecond)

	real, err := net.DialTimeout("tcp4", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("port closed after an idle connection: %v", err)
	}
	defer real.Close()
	_ = real.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := real.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 10)
	if _, err := io.ReadFull(real, got); err != nil || string(got) != "echo:hello" {
		t.Fatalf("real request blocked behind the idle connection: %q, %v", got, err)
	}
}
