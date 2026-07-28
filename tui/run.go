package tui

import (
	"context"
	"errors"
	"os"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-isatty"
)

// Supported reports whether this process can host the dashboard.
//
// Both directions are required, and for different reasons. Without a terminal
// on stdout the frames go into a pipe or a file as thousands of escape
// sequences per minute. Without one on stdin there is no way to press a key,
// so the dashboard would render forever with no way to quit but a signal.
//
// Checked rather than attempted: bubbletea will start on a non-terminal and
// produce exactly that mess, and `slimproxy > log 2>&1` under a service
// manager is a completely ordinary way to run this.
func Supported() bool {
	return isTerminal(os.Stdout) && isTerminal(os.Stdin)
}

func isTerminal(f *os.File) bool {
	fd := f.Fd()
	// isatty.IsCygwinTerminal covers MSYS2, Git Bash and Cygwin on Windows,
	// where the handle is a named pipe rather than a console and the plain
	// check returns false for what is genuinely a terminal.
	return isatty.IsTerminal(fd) || isatty.IsCygwinTerminal(fd)
}

// Run displays the dashboard until the operator quits or ctx is cancelled.
//
// Returns nil on a normal quit, including a cancelled context: the operator
// stopping the program is not a failure, and returning one would make every
// Ctrl-C exit non-zero.
// out is where frames are drawn. It must be the real terminal even when the
// process has redirected os.Stdout elsewhere -- which is exactly what the host
// does to keep third-party fmt.Printf calls out of the frame. Passing nil falls
// back to os.Stdout.
func Run(ctx context.Context, deps Deps, out *os.File) error {
	opts := []tea.ProgramOption{
		// The alternate screen is what makes this a dashboard rather than
		// scrollback: on quit the terminal is restored to exactly what was
		// there before, with no thousand-frame history to scroll through.
		tea.WithAltScreen(),
		tea.WithContext(ctx),
	}
	if out != nil {
		opts = append(opts, tea.WithOutput(out))
	}
	p := tea.NewProgram(New(ctx, deps), opts...)
	if _, err := p.Run(); err != nil {
		if errors.Is(err, tea.ErrProgramKilled) || errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}
	return nil
}
