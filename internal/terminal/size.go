package terminal

import "fmt"

// MaxTerminalDimension is the largest size supported consistently by Unix
// PTYs and Windows ConPTY, whose COORD fields are signed 16-bit integers.
const MaxTerminalDimension = 32767

// NormalizeDimensions applies the documented 80x24 defaults for zero values
// and rejects dimensions that cannot be represented by every PTY backend.
func NormalizeDimensions(cols, rows int) (int, int, error) {
	if cols < 0 || cols > MaxTerminalDimension {
		return 0, 0, fmt.Errorf("terminal columns must be between 0 and %d", MaxTerminalDimension)
	}
	if rows < 0 || rows > MaxTerminalDimension {
		return 0, 0, fmt.Errorf("terminal rows must be between 0 and %d", MaxTerminalDimension)
	}
	if cols == 0 {
		cols = 80
	}
	if rows == 0 {
		rows = 24
	}
	return cols, rows, nil
}
