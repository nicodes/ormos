//go:build darwin && !ios

package system

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
)

const darwinNetstatFixture = `Active Internet connections (including servers)
Proto Recv-Q Send-Q  Local Address          Foreign Address        (state)
tcp4       0      0  127.0.0.1.4100         *.*                    LISTEN
tcp4       0      0  *.4200                  *.*                    LISTEN
tcp6       0      0  ::1.4300                *.*                    LISTEN
tcp6       0      0  *.4200                  *.*                    LISTEN
tcp4       0      0  192.0.2.1.4400          *.*                    LISTEN
tcp6       0      0  2001:db8::1.4500        *.*                    LISTEN
tcp4       0      0  127.0.0.1.4600         127.0.0.1.9999         ESTABLISHED
`

func TestDiscoverDarwinPortsParsesLoopbackAndWildcard(t *testing.T) {
	run := func(_ context.Context, _ string) (io.Reader, error) {
		return strings.NewReader(darwinNetstatFixture), nil
	}
	got, err := discoverDarwinPorts(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	want := []int{4100, 4200, 4300}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("discoverDarwinPorts() = %v, want %v", got, want)
	}
}

func TestDiscoverDarwinPortsKeepsOtherFamilyWhenOneIsEmpty(t *testing.T) {
	run := func(_ context.Context, family string) (io.Reader, error) {
		if family == "inet6" {
			return strings.NewReader("\n"), nil
		}
		return strings.NewReader(`Active Internet connections (including servers)
Proto Recv-Q Send-Q  Local Address          Foreign Address        (state)
tcp4       0      0  127.0.0.1.4100         *.*                    LISTEN
`), nil
	}
	got, err := discoverDarwinPorts(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	want := []int{4100}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("discoverDarwinPorts() = %v, want %v", got, want)
	}
}

func TestDiscoverDarwinPortsFailuresAreExplicit(t *testing.T) {
	tests := []struct {
		name string
		run  darwinNetstatRunner
		want string
	}{
		{
			name: "tool unavailable",
			run: func(context.Context, string) (io.Reader, error) {
				return nil, errors.New("executable not found")
			},
			want: "netstat inet: executable not found",
		},
		{
			name: "timeout",
			run: func(context.Context, string) (io.Reader, error) {
				return nil, context.DeadlineExceeded
			},
			want: "netstat inet: context deadline exceeded",
		},
		{
			name: "malformed output",
			run: func(context.Context, string) (io.Reader, error) {
				return strings.NewReader("not a socket table\n"), nil
			},
			want: "netstat inet output: missing documented socket table header",
		},
		{
			name: "malformed listener",
			run: func(context.Context, string) (io.Reader, error) {
				return strings.NewReader("Proto Recv-Q Send-Q Local Address Foreign Address (state)\n" +
					"tcp4 0 0 127.0.0.1.not-a-port *.* LISTEN\n"), nil
			},
			want: `netstat inet output: malformed LISTEN row "tcp4 0 0 127.0.0.1.not-a-port *.* LISTEN": invalid local port "not-a-port"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ports, err := discoverDarwinPorts(context.Background(), tt.run)
			if err == nil || err.Error() != tt.want {
				t.Fatalf("discoverDarwinPorts() = (%v, %v), want nil, %q", ports, err, tt.want)
			}
			if ports != nil {
				t.Fatalf("failure returned ports %v, want nil", ports)
			}
		})
	}
}

func TestCappedBufferDrainsAndCaps(t *testing.T) {
	b := cappedBuffer{max: 4}
	if n, err := b.Write([]byte("123456")); n != 6 || err != nil {
		t.Fatalf("Write() = (%d, %v), want (6, nil)", n, err)
	}
	if got := b.String(); got != "1234" || !b.overflow {
		t.Fatalf("capped buffer = (%q, overflow %v), want (\"1234\", true)", got, b.overflow)
	}
}
