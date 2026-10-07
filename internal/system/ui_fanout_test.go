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
