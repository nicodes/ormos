//go:build (linux && !android) || (darwin && !ios)

package system

import "errors"

// Sizes share the output byte clock: replay must interpret cursor movements
// using the grid on which they were emitted, before fitting today's viewport.
type uiTerminalSize struct {
	Offset uint64 `json:"offset"`
	Cols   uint16 `json:"cols"`
	Rows   uint16 `json:"rows"`
}

func (t *uiTerminal) sizeAt(cursor uint64) uiTerminalSize {
	size := uiTerminalSize{Cols: 80, Rows: 24}
	for _, next := range t.sizes {
		if next.Offset > cursor {
			break
		}
		size = next
	}
	return size
}

func (t *uiTerminal) trimSizes() {
	start := t.end - uint64(t.history.size)
	i := 0
	for i+1 < len(t.sizes) && t.sizes[i+1].Offset <= start {
		i++
	}
	if i > 0 {
		copy(t.sizes, t.sizes[i:])
		t.sizes = t.sizes[:len(t.sizes)-i]
	}
}

func (t *uiTerminal) setSize(cols, rows uint16) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.resize == nil {
		return errors.New("terminal cannot resize")
	}
	previous := t.sizeAt(t.end)
	if previous.Cols == cols && previous.Rows == rows {
		return nil
	}
	if err := t.resize(cols, rows); err != nil {
		return err
	}
	size := uiTerminalSize{Offset: t.end, Cols: cols, Rows: rows}
	if n := len(t.sizes); n > 0 && t.sizes[n-1].Offset == t.end {
		t.sizes[n-1] = size
	} else {
		t.sizes = append(t.sizes, size)
	}
	for update := range t.updates {
		select {
		case update <- struct{}{}:
		default:
		}
	}
	return nil
}

// Bound a delivery at the next grid change, including when output is served
// from the shared chunk cache. Called under t.mu.
func (t *uiTerminal) geometryLimit(cursor uint64, limit int) int {
	for _, size := range t.sizes {
		if size.Offset > cursor {
			return min(limit, int(size.Offset-cursor))
		}
	}
	return limit
}
