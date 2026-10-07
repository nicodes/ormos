//go:build (linux && !android) || (darwin && !ios)

package system

import (
	"bytes"
	"fmt"
	"testing"
)

func TestUITerminalOutputOwnsReadBuffer(t *testing.T) {
	a, b := make(chan []byte, 1), make(chan []byte, 1)
	term := &uiTerminal{readers: map[chan []byte]bool{a: true, b: true}}
	data := []byte("original output")
	term.append(data)
	copy(data, "reused PTY read!")
	for _, reader := range []chan []byte{a, b} {
		if got := <-reader; string(got) != "original output" {
			t.Fatalf("PTY buffer reuse changed queued output: %q", got)
		}
	}
	if term.output() != "original output" {
		t.Fatal("PTY buffer reuse changed retained history")
	}
}

func TestUITerminalSlowReaderDoesNotBlockOthers(t *testing.T) {
	slow, fast := make(chan []byte, 1), make(chan []byte, 1)
	term := &uiTerminal{readers: map[chan []byte]bool{slow: true, fast: true}}
	term.append([]byte("first"))
	<-fast
	term.append([]byte("second"))
	if term.readers[slow] || !term.readers[fast] {
		t.Fatal("only the full reader should be evicted")
	}
	if got := string(<-fast); got != "second" {
		t.Fatal(got)
	}
	<-slow
	if _, open := <-slow; open {
		t.Fatal("slow reader must reconnect for retained history")
	}
	if term.output() != "firstsecond" {
		t.Fatal("reader eviction lost retained output")
	}
}

func BenchmarkUITerminalFanout(b *testing.B) {
	for _, count := range []int{0, 1, 8} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			term := &uiTerminal{readers: map[chan []byte]bool{}}
			for i := 0; i < count; i++ {
				term.readers[make(chan []byte, 1)] = true
			}
			data := bytes.Repeat([]byte("x"), 8192)
			term.history.buf = make([]byte, uiTerminalBufMax)
			b.ReportAllocs()
			b.SetBytes(int64(len(data)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				term.append(data)
				for reader := range term.readers {
					<-reader
				}
			}
		})
	}
}

func BenchmarkUITerminalFirstOutput(b *testing.B) {
	data := []byte("shell prompt\r\n")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		term := &uiTerminal{}
		term.append(data)
	}
}

func TestUIHistoryGrowsWithoutChangingReplay(t *testing.T) {
	term := &uiTerminal{}
	term.append(nil)
	if term.history.buf != nil {
		t.Fatal("empty output allocated history")
	}
	term.append([]byte("prompt"))
	if len(term.history.buf) > 8192 {
		t.Fatal("small terminal reserved its full history bound")
	}
	expected, end := []byte("prompt"), uint64(6)
	for _, length := range []int{8191, 1, 8192, 110003, 1 << 20, uiTerminalBufMax - 17, 257, uiTerminalBufMax + 13} {
		data := make([]byte, length)
		for i := range data {
			data[i] = byte(i*31 + length)
		}
		term.append(data)
		expected = append(expected, data...)
		if len(expected) > uiTerminalBufMax {
			expected = expected[len(expected)-uiTerminalBufMax:]
		}
		end += uint64(len(data))
		got, start, reset := term.replay(nil)
		if !bytes.Equal(got, expected) || !reset || start != end-uint64(len(expected)) || term.end != end {
			t.Fatalf("growth at %d changed byte order or offsets", length)
		}
		if len(term.history.buf) < len(expected) || len(term.history.buf) > uiTerminalBufMax {
			t.Fatal("history allocation escaped its bounds")
		}
	}
}

func TestUIHistoryGrowthPreservesWrappedBuffer(t *testing.T) {
	term := &uiTerminal{history: replayRing{buf: make([]byte, 8)}, end: 10}
	term.history.append([]byte("abcdefghij"))
	term.append([]byte("klmnop"))
	if got := term.output(); got != "cdefghijklmnop" {
		t.Fatalf("wrapped growth = %q", got)
	}
	_, start, _ := term.replay(nil)
	if start != 2 {
		t.Fatalf("growth invented earlier history: start=%d", start)
	}
}
