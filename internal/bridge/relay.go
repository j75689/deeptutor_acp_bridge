package bridge

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ParsePortRange parses "lo-hi" (or a single port) into an inclusive range.
func ParsePortRange(s string) (lo, hi int, err error) {
	a, b, found := strings.Cut(strings.TrimSpace(s), "-")
	if lo, err = strconv.Atoi(strings.TrimSpace(a)); err != nil {
		return 0, 0, fmt.Errorf("invalid port range %q", s)
	}
	hi = lo
	if found {
		if hi, err = strconv.Atoi(strings.TrimSpace(b)); err != nil {
			return 0, 0, fmt.Errorf("invalid port range %q", s)
		}
	}
	if lo < 1 || hi > 65535 || lo > hi {
		return 0, 0, fmt.Errorf("invalid port range %q", s)
	}
	return lo, hi, nil
}

const relayPollEvery = 250 * time.Millisecond

// Relay exposes the agent's loopback-only OAuth callback listener on the
// container's network address so a published port range reaches it.
//
// It is off until SetActive(true), which the bridge does only while an
// authenticate call is in flight, and each port is relayed for a single
// connection. The agent opens other loopback services after sign-in (a local
// credentials-injection proxy among them); those must never be reachable from
// the container network or the published ports.
//
// It must not hold the whole range: the kernel also draws outgoing connections'
// source ports from the (deliberately narrow) ephemeral range, and skips ports
// that anything has bound. So the relay watches for a 127.0.0.1 listener inside
// [Lo, Hi] and binds the interface address on that one port only while it
// exists. Binding the interface address rather than 0.0.0.0 leaves
// 127.0.0.1:<port> to the agent.
type Relay struct {
	Lo, Hi    int
	BindHosts []string // default: the host's non-loopback IPv4 addresses
	// Scan reports the ports in [Lo, Hi] the agent is listening on, on loopback.
	// Default: read /proc/net/tcp (Linux).
	Scan func() ([]int, error)

	mu     sync.Mutex
	on     bool         // SetActive state
	used   map[int]bool // ports already relayed once in this active window
	active map[int][]net.Listener
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// SetActive opens or closes the relay. Closing releases every port; opening
// forgets which ports were already used, so each sign-in starts fresh.
func (r *Relay) SetActive(on bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.on = on
	r.used = map[int]bool{}
	if !on {
		for port := range r.active {
			r.release(port)
		}
	}
}

// Start begins watching and relaying until Close.
func (r *Relay) Start(ctx context.Context) error {
	if len(r.BindHosts) == 0 {
		hosts, err := nonLoopbackIPv4()
		if err != nil {
			return err
		}
		r.BindHosts = hosts
	}
	if len(r.BindHosts) == 0 {
		return errors.New("oauth relay: no non-loopback IPv4 address to bind")
	}
	if r.Scan == nil {
		r.Scan = func() ([]int, error) { return procLoopbackListeners(r.Lo, r.Hi) }
	}
	if _, err := r.Scan(); err != nil {
		return fmt.Errorf("oauth relay: %w", err)
	}
	r.active = map[int][]net.Listener{}
	r.used = map[int]bool{}
	ctx, r.cancel = context.WithCancel(ctx)
	r.wg.Add(1)
	go r.watch(ctx)
	slog.Info("oauth relay watching", "ports", fmt.Sprintf("%d-%d", r.Lo, r.Hi), "hosts", r.BindHosts)
	return nil
}

// Close stops watching and releases every relayed port. Listeners are closed
// before waiting: an accept goroutine only returns once its listener is closed.
func (r *Relay) Close() {
	if r.cancel != nil {
		r.cancel()
	}
	r.mu.Lock()
	r.on = false
	for port := range r.active {
		r.release(port)
	}
	r.mu.Unlock()
	r.wg.Wait()
}

func (r *Relay) watch(ctx context.Context) {
	defer r.wg.Done()
	t := time.NewTicker(relayPollEvery)
	defer t.Stop()
	for {
		r.sync(ctx)
		select {
		case <-t.C:
		case <-ctx.Done():
			return
		}
	}
}

func (r *Relay) sync(ctx context.Context) {
	ports, err := r.Scan()
	if err != nil {
		slog.Warn("oauth relay: scan failed", "err", err)
		return
	}
	want := map[int]bool{}
	for _, p := range ports {
		want[p] = true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.on {
		return
	}
	for port := range r.active {
		if !want[port] {
			r.release(port)
			slog.Info("oauth relay: released", "port", port)
		}
	}
	for port := range want {
		if _, ok := r.active[port]; ok || r.used[port] {
			continue
		}
		var lns []net.Listener
		for _, host := range r.BindHosts {
			ln, err := net.Listen("tcp4", net.JoinHostPort(host, strconv.Itoa(port)))
			if err != nil {
				slog.Warn("oauth relay: cannot bind", "host", host, "port", port, "err", err)
				continue
			}
			lns = append(lns, ln)
			r.wg.Add(1)
			go r.accept(ctx, ln, port)
		}
		if len(lns) > 0 {
			r.active[port] = lns
			slog.Info("oauth relay: relaying", "port", port)
		}
	}
}

// release closes the listeners for port; the caller holds r.mu.
func (r *Relay) release(port int) {
	for _, ln := range r.active[port] {
		_ = ln.Close()
	}
	delete(r.active, port)
}

func (r *Relay) accept(ctx context.Context, ln net.Listener, port int) {
	defer r.wg.Done()
	target := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			d := net.Dialer{Timeout: 2 * time.Second}
			up, err := d.DialContext(ctx, "tcp4", target)
			if err != nil {
				return // the agent stopped listening between the poll and now
			}
			defer up.Close()
			done := make(chan struct{}, 2)
			go func() { _, _ = io.Copy(up, c); done <- struct{}{} }()
			go func() { _, _ = io.Copy(c, up); done <- struct{}{} }()
			<-done
			// The sign-in callback is one request; do not keep the port open.
			r.mu.Lock()
			if r.on {
				r.used[port] = true
			}
			r.release(port)
			r.mu.Unlock()
		}()
	}
}

// procLoopbackListeners lists ports in [lo, hi] with a LISTEN socket on 127.0.0.1.
func procLoopbackListeners(lo, hi int) ([]int, error) {
	f, err := os.Open("/proc/net/tcp")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []int
	sc := bufio.NewScanner(f)
	sc.Scan() // header
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 4 || fields[3] != "0A" { // 0A = LISTEN
			continue
		}
		addr, portHex, ok := strings.Cut(fields[1], ":")
		if !ok || addr != "0100007F" { // 127.0.0.1, little-endian
			continue
		}
		p, err := strconv.ParseInt(portHex, 16, 32)
		if err != nil || int(p) < lo || int(p) > hi {
			continue
		}
		out = append(out, int(p))
	}
	return out, sc.Err()
}

func nonLoopbackIPv4() ([]string, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok || ipn.IP.IsLoopback() || ipn.IP.To4() == nil {
			continue
		}
		out = append(out, ipn.IP.String())
	}
	return out, nil
}
