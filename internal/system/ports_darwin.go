//go:build darwin && !ios

package system

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	darwinDiscoveryTimeout = 2 * time.Second
	darwinWaitDelay        = 250 * time.Millisecond
	darwinNetstatMaxOutput = 1 << 20
)

type darwinNetstatRunner func(context.Context, string) (io.Reader, error)

// listeningPorts uses macOS's base-system netstat. Apple's versioned Darwin
// netstat(1) documentation defines -a, -n, -f inet/inet6, the local
// address field, wildcard rendering, and LISTEN. It does not promise a stable
// machine-readable schema, so parsing below validates the documented columns
// and fails closed if their shape is not recognizable.
func listeningPorts() ([]int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), darwinDiscoveryTimeout)
	defer cancel()
	return discoverDarwinPorts(ctx, runDarwinNetstat)
}

func discoverDarwinPorts(ctx context.Context, run darwinNetstatRunner) ([]int, error) {
	set := make(map[int]struct{})
	for _, family := range []string{"inet", "inet6"} {
		out, err := run(ctx, family)
		if err != nil {
			return nil, fmt.Errorf("netstat %s: %w", family, err)
		}
		if err := parseDarwinNetstat(out, set); err != nil {
			return nil, fmt.Errorf("netstat %s output: %w", family, err)
		}
	}
	ports := make([]int, 0, len(set))
	for port := range set {
		ports = append(ports, port)
	}
	sort.Ints(ports)
	return ports, nil
}

func runDarwinNetstat(ctx context.Context, family string) (io.Reader, error) {
	var stdout, stderr cappedBuffer
	stdout.max = darwinNetstatMaxOutput
	stderr.max = darwinNetstatMaxOutput
	// netstat(1)'s synopsis makes -f and -p alternatives. Select each address
	// family with -f and accept only tcp rows in the parser rather than relying
	// on the undocumented combination of both flags.
	cmd := exec.CommandContext(ctx, "/usr/sbin/netstat", "-anl", "-f", family)
	cmd.Env = []string{"LC_ALL=C", "LANG=C"}
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.WaitDelay = darwinWaitDelay
	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("run /usr/sbin/netstat: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	if stdout.overflow {
		return nil, fmt.Errorf("output exceeds %d bytes", darwinNetstatMaxOutput)
	}
	return strings.NewReader(stdout.String()), nil
}

// cappedBuffer keeps a bounded prefix while reporting successful writes so
// os/exec continues draining the child pipe instead of deadlocking the child.
type cappedBuffer struct {
	b        strings.Builder
	max      int
	overflow bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	remaining := b.max - b.b.Len()
	if remaining > len(p) {
		remaining = len(p)
	}
	if remaining > 0 {
		_, _ = b.b.Write(p[:remaining])
	}
	if remaining < len(p) {
		b.overflow = true
	}
	return len(p), nil
}

func (b *cappedBuffer) String() string { return b.b.String() }

func parseDarwinNetstat(r io.Reader, set map[int]struct{}) error {
	scanner := bufio.NewScanner(r)
	sawHeader := false
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 {
			continue
		}
		if fields[0] == "Proto" {
			sawHeader = len(fields) >= 5 && fields[1] == "Recv-Q" && fields[2] == "Send-Q"
			continue
		}
		if !strings.HasPrefix(fields[0], "tcp") {
			continue
		}
		if len(fields) < 6 {
			return fmt.Errorf("malformed TCP row %q", scanner.Text())
		}
		if fields[len(fields)-1] != "LISTEN" {
			continue
		}
		host, port, err := parseDarwinLocal(fields[3])
		if err != nil {
			return fmt.Errorf("malformed LISTEN row %q: %w", scanner.Text(), err)
		}
		if isDarwinLoopbackOrWildcard(host) {
			set[port] = struct{}{}
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if !sawHeader {
		return errors.New("missing documented socket table header")
	}
	return nil
}

func parseDarwinLocal(local string) (string, int, error) {
	dot := strings.LastIndexByte(local, '.')
	if dot <= 0 || dot == len(local)-1 {
		return "", 0, errors.New("local address is not host.port")
	}
	port, err := strconv.Atoi(local[dot+1:])
	if err != nil || port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("invalid local port %q", local[dot+1:])
	}
	return local[:dot], port, nil
}

func isDarwinLoopbackOrWildcard(host string) bool {
	switch host {
	case "127.0.0.1", "*", "0.0.0.0", "::1", "::":
		return true
	default:
		return false
	}
}
