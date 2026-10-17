package live_test

import (
	"os"
	"os/signal"
)

func signalNotify(ch chan os.Signal) { signal.Notify(ch, os.Interrupt) }
