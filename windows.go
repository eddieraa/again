// +build windows

package again

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

func (a *Again) Env() (m map[string]string, err error) {
	var fds []string
	var names []string
	var fdNames []string
	a.services.Range(func(k, value interface{}) bool {
		s := value.(*Service)
		names = append(names, s.Name)

		fds = append(fds, fmt.Sprint(s.Descriptor))
		fdNames = append(fdNames, s.FdName)
		return true
	})
	if err != nil {
		return
	}
	return map[string]string{
		"GOAGAIN_FD":           strings.Join(fds, ","),
		"GOAGAIN_SERVICE_NAME": strings.Join(names, ","),
		"GOAGAIN_NAME":         strings.Join(fdNames, ","),
	}, nil
}

// Kill process specified in the environment with the signal specified in the
// environment; default to SIGQUIT.
func Kill() error {
	var (
		pid int
		sig syscall.Signal
	)
	_, err := fmt.Sscan(os.Getenv("GOAGAIN_PID"), &pid)
	if io.EOF == err {
		_, err = fmt.Sscan(os.Getenv("GOAGAIN_PPID"), &pid)
	}
	if nil != err {
		return err
	}
	if _, err := fmt.Sscan(os.Getenv("GOAGAIN_SIGNAL"), &sig); nil != err {
		sig = syscall.SIGQUIT
	}

	process, err := os.FindProcess(int(pid))
	if nil != err {
		return err
	}
	log.Println("sending signal", sig, "to process", pid)
	return process.Signal(sig)
}

// Wait waits for signals
func Wait(a *Again) (syscall.Signal, error) {
	ch := make(chan os.Signal, 2)
	signal.Notify(
		ch,
		syscall.SIGHUP,
		syscall.SIGINT,
		syscall.SIGQUIT,
		syscall.SIGTERM,
	)

	for {
		sig := <-ch
		log.Println(sig.String())
		switch sig {

		// SIGHUP should reload configuration.
		case syscall.SIGHUP:
			if a.Hooks.OnSIGHUP != nil {
				if err := a.Hooks.OnSIGHUP(a); err != nil {
					log.Println("OnSIGHUP:", err)
				}
			}

		// SIGINT should exit.
		case syscall.SIGINT:
			return syscall.SIGINT, nil

		// SIGQUIT should exit gracefully.
		case syscall.SIGQUIT:
			if a.Hooks.OnSIGQUIT != nil {
				if err := a.Hooks.OnSIGQUIT(a); err != nil {
					log.Println("OnSIGQUIT:", err)
				}
			}
			return syscall.SIGQUIT, nil

		// SIGTERM should exit.
		case syscall.SIGTERM:
			if a.Hooks.OnSIGTERM != nil {
				if err := a.Hooks.OnSIGHUP(a); err != nil {
					log.Println("OnSIGTERM:", err)
				}
			}
			return syscall.SIGTERM, nil

		}
	}
}
