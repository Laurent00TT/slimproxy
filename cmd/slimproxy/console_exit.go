package main

import (
	"bufio"
	"fmt"
	"io"

	"github.com/Laurent00TT/slimproxy/i18n"
)

// Explorer closes a console's last process immediately, taking its startup
// error with it. Only a dedicated interactive console needs acknowledgement;
// waiting in an existing shell or a service would hang scripts and supervisors.
func waitForConsoleError(standalone bool, input io.Reader, output io.Writer) {
	if !standalone {
		return
	}
	fmt.Fprintln(output, i18n.T("\n按回车键关闭窗口。", "\nPress Enter to close this window."))
	_, _ = bufio.NewReader(input).ReadString('\n')
}
