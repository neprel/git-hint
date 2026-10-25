// Command gate is the HINT model gate (07B-J §20): the only path of an agent container to
// the model proxy. It listens on one TCP port of its address in the agent network A only,
// and forwards each connection to one target that it resolves once at start (from the
// container's hosts file); it forwards nothing else, runs no other service and keeps
// bounded deadlines. It is built from this file alone (standard library) into a scratch
// image by the container runner. Two more modes serve the runner's network probe only:
// -sentinel (a controlled resolver and TCP listener that counts what reaches it) and
// -probe (checks from inside a container with the agent's network settings).
package main

import (
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "-sentinel" {
		sentinel()
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "-probe" {
		probe(os.Args[2:])
		return
	}
	listenNet := flag.String("listen-net", "", "CIDR of the agent network A; the gate listens on its own address in it")
	port := flag.Int("port", 8080, "listening port")
	target := flag.String("target", "", "host:port of the model proxy, resolved once")
	maxConns := flag.Int("max-conns", 32, "concurrent connections")
	connectSec := flag.Int("connect-seconds", 5, "connect deadline to the target")
	idleSec := flag.Int("idle-seconds", 120, "a connection without traffic for this long is closed")
	lifeSec := flag.Int("max-seconds", 3600, "the gate stops after this long")
	flag.Parse()
	_, cidr, err := net.ParseCIDR(*listenNet)
	if err != nil {
		fail("listen-net")
	}
	host, tport, err := net.SplitHostPort(*target)
	if err != nil {
		fail("target")
	}
	ips, err := net.LookupIP(host)
	var to net.IP
	for _, ip := range ips {
		if ip.To4() != nil {
			to = ip.To4()
			break
		}
	}
	if err != nil || to == nil {
		fail("target not resolvable")
	}
	var bind net.IP
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && cidr.Contains(n.IP) {
			bind = n.IP.To4()
		}
	}
	if bind == nil {
		fail("no address in the agent network")
	}
	ln, err := net.Listen("tcp4", net.JoinHostPort(bind.String(), fmt.Sprint(*port)))
	if err != nil {
		fail("listen")
	}
	dst := net.JoinHostPort(to.String(), tport)
	fmt.Printf("HINT_GATE listen=%s target=%s\n", ln.Addr(), dst)
	go func() { time.Sleep(time.Duration(*lifeSec) * time.Second); os.Exit(0) }()
	sem := make(chan struct{}, *maxConns)
	for {
		c, err := ln.Accept()
		if err != nil {
			continue
		}
		select {
		case sem <- struct{}{}:
		default:
			c.Close() // over the connection limit
			continue
		}
		go func() {
			defer func() { <-sem }()
			forward(c, dst, time.Duration(*connectSec)*time.Second, time.Duration(*idleSec)*time.Second)
		}()
	}
}

// forward copies both ways between the client and the one target. Traffic in either
// direction renews the connection's idle window. Each write retains its own bound.
func forward(c net.Conn, dst string, connect, idle time.Duration) {
	u, err := net.DialTimeout("tcp4", dst, connect)
	if err != nil {
		c.Close()
		return
	}
	forwardConnected(c, u, idle, wallIdleClock{})
}

type idleTimer interface {
	channel() <-chan time.Time
	reset(time.Duration)
	stop()
}
type idleClock interface {
	now() time.Time
	timer(time.Duration) idleTimer
}
type wallIdleClock struct{}
type wallIdleTimer struct{ *time.Timer }

func (wallIdleClock) now() time.Time                  { return time.Now() }
func (wallIdleClock) timer(d time.Duration) idleTimer { return wallIdleTimer{time.NewTimer(d)} }
func (t wallIdleTimer) channel() <-chan time.Time     { return t.C }
func (t wallIdleTimer) reset(d time.Duration)         { t.Reset(d) }
func (t wallIdleTimer) stop()                         { t.Stop() }

// The calling goroutine owns the watchdog and joins both copy workers. No timer can
// close a connection using an activity sample older than the last synchronized read.
func forwardConnected(c, u net.Conn, idle time.Duration, clock idleClock) {
	var mu sync.Mutex
	last, closed := clock.now(), false
	var once sync.Once
	closeBoth := func() {
		mu.Lock()
		closed = true
		mu.Unlock()
		once.Do(func() { c.Close(); u.Close() })
	}
	defer closeBoth()
	changed, done := make(chan struct{}, 1), make(chan struct{}, 2)
	activity := func() {
		mu.Lock()
		if !closed {
			last = clock.now()
		}
		mu.Unlock()
		select {
		case changed <- struct{}{}:
		default:
		}
	}
	timer := clock.timer(idle)
	defer timer.stop()
	pipe := func(w, r net.Conn) {
		defer func() { closeBoth(); done <- struct{}{} }()
		buf := make([]byte, 32<<10)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				activity()
				w.SetWriteDeadline(clock.now().Add(idle))
				if _, werr := w.Write(buf[:n]); werr != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
	}
	go pipe(u, c)
	go pipe(c, u)
	for workers := 0; workers < 2; {
		select {
		case <-done:
			workers++
		case <-changed:
		case <-timer.channel():
		}
		mu.Lock()
		remaining := last.Add(idle).Sub(clock.now())
		expired := !closed && remaining <= 0
		if expired {
			closed = true
		}
		mu.Unlock()
		if expired {
			closeBoth()
		} else if remaining > 0 {
			timer.reset(remaining)
		}
	}
}

func fail(why string) {
	fmt.Fprintln(os.Stderr, "HINT_GATE error:", why)
	os.Exit(2)
}

// sentinel answers TCP on port 8081 with a marker and counts every DNS query on UDP 53 (it
// never answers); each event is one stdout line.
func sentinel() {
	go func() {
		ln, err := net.Listen("tcp4", ":8081")
		if err != nil {
			fail("sentinel tcp")
		}
		for {
			c, err := ln.Accept()
			if err != nil {
				continue
			}
			fmt.Println("HINT_SENTINEL tcp")
			io.WriteString(c, "HINT-SENTINEL-OK\n")
			c.Close()
		}
	}()
	pc, err := net.ListenPacket("udp4", ":53")
	if err != nil {
		fail("sentinel udp")
	}
	fmt.Println("HINT_SENTINEL ready")
	buf := make([]byte, 4096)
	for {
		if _, _, err := pc.ReadFrom(buf); err == nil {
			fmt.Println("HINT_SENTINEL dns")
		}
	}
}

// probe runs the checks named by its arguments and prints one line per check:
// "PROBE <name> ok" or "PROBE <name> fail". reach=NAME=HOST:PORT=MARKER must read MARKER
// from HOST:PORT; deny=NAME=HOST:PORT must not connect; dns=NAME=HOST must not resolve;
// the identity checks (uid, capabilities, no-new-privileges, socket, IPv6) always run.
func probe(args []string) {
	for _, a := range args {
		kind, rest, _ := strings.Cut(a, "=")
		name, spec, _ := strings.Cut(rest, "=")
		ok := false
		switch kind {
		case "reach":
			addr, marker, _ := strings.Cut(spec, "=")
			ok = readMarker(addr, marker)
		case "deny":
			c, err := net.DialTimeout("tcp", spec, 3*time.Second)
			ok = err != nil
			if c != nil {
				c.Close()
			}
		case "dns":
			_, err := net.LookupHost(spec)
			ok = err != nil
		}
		report(kind+":"+name, ok)
	}
	b, _ := os.ReadFile("/proc/self/status")
	st := strings.Split(string(b), "\n")
	report("uid", slices.Contains(st, "Uid:\t65534\t65534\t65534\t65534"))
	report("cap_eff", slices.Contains(st, "CapEff:\t0000000000000000"))
	report("cap_bnd", slices.Contains(st, "CapBnd:\t0000000000000000"))
	report("no_new_privs", slices.Contains(st, "NoNewPrivs:\t1"))
	_, err := os.Stat("/var/run/docker.sock")
	report("no_docker_socket", os.IsNotExist(err))
	v6, _ := os.ReadFile("/proc/net/if_inet6")
	global := false
	for _, ln := range strings.Split(string(v6), "\n") {
		// fields: address, index, prefix length, scope (00 global), flags, device
		f := strings.Fields(ln)
		if len(f) >= 4 && f[3] == "00" && f[0] != "00000000000000000000000000000001" {
			global = true
		}
	}
	report("no_global_ipv6", !global)
}

func readMarker(addr, marker string) bool {
	c, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return false
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	io.WriteString(c, "GET / HTTP/1.0\r\n\r\n")
	b, _ := io.ReadAll(io.LimitReader(c, 4096))
	return len(marker) > 0 && strings.Contains(string(b), marker)
}

func report(name string, ok bool) {
	st := "fail"
	if ok {
		st = "ok"
	}
	fmt.Printf("PROBE %s %s\n", name, st)
}
