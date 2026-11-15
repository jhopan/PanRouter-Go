package daemon

import (
	"io"
	stdlog "log"
	"os"
)

// AttachConsoleLog mirrors this process's console output into the run log.
//
// A daemon started by `panrouter-go start` inherits the log as its stdout, so it
// needs nothing here — and would print every line twice if it called this. A
// process started by a service manager is different: on Windows the scheduled
// task gives it no writable stdout at all, so every request line went nowhere.
// The operator's `panrouter-go logs` then showed a file that had stopped at the
// previous daemon's "Server stopped gracefully", and the request lines that say
// which account served what were lost.
//
// The returned function restores the previous output and closes the file. A log
// that cannot be opened is not fatal: the process keeps its own stdout, which is
// what a foreground run wants anyway.
func AttachConsoleLog() (func(), error) {
	file, err := openRunLog()
	if err != nil {
		return func() {}, err
	}
	stdlog.SetOutput(io.MultiWriter(os.Stdout, file))
	return func() {
		stdlog.SetOutput(os.Stderr)
		_ = file.Close()
	}, nil
}
