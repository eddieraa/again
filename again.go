package again

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"syscall"
)

var OnForkHook func()

// Don't make the caller import syscall.
const (
	SIGINT  = syscall.SIGINT
	SIGQUIT = syscall.SIGQUIT
	SIGTERM = syscall.SIGTERM
)

// Service is a single service listening on a single net.Listener.
type Service struct {
	Name       string
	FdName     string
	Descriptor uintptr
	Listener   net.Listener
}

// Hooks callbacks invoked when specific signal is received.
type Hooks struct {
	// OnSIGHUP is the function called when the server receives a SIGHUP
	// signal. The normal use case for SIGHUP is to reload the
	// configuration.
	OnSIGHUP func(*Again) error
	// OnSIGUSR1 is the function called when the server receives a
	// SIGUSR1 signal. The normal use case for SIGUSR1 is to repon the
	// log files.
	OnSIGUSR1 func(*Again) error
	// OnSIGQUIT use this for graceful shutdown
	OnSIGQUIT func(*Again) error
	OnSIGTERM func(*Again) error
}

// Again manages services that need graceful restarts
type Again struct {
	services *sync.Map
	Hooks    Hooks
}

func New(hooks ...Hooks) Again {
	var h Hooks
	if len(hooks) > 0 {
		h = hooks[0]
	}
	return Again{
		services: &sync.Map{},
		Hooks:    h,
	}
}

func ListerName(l net.Listener) string {
	addr := l.Addr()
	return fmt.Sprintf("%s:%s->", addr.Network(), addr.String())
}

func (a *Again) Range(fn func(*Service)) {
	a.services.Range(func(k, v interface{}) bool {
		s := v.(*Service)
		fn(s)
		return true
	})
}

// Close tries to close all service listeners
func (a Again) Close() error {
	var e bytes.Buffer
	a.Range(func(s *Service) {
		if err := s.Listener.Close(); err != nil {
			e.WriteString(err.Error())
			e.WriteByte('\n')
		}
	})
	if e.Len() > 0 {
		return errors.New(e.String())
	}
	return nil
}
func hasElem(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Ptr, reflect.Interface:
		return true
	default:
		return false
	}
}

// Listen creates a new service with the given listener.
func (a *Again) Listen(name string, ls net.Listener) error {
	v := reflect.ValueOf(ls)
	if v.Kind() == reflect.Ptr {
		v = v.Elem()
	}
	// check if we have net.Listener embedded. Its a workaround to support
	// crypto/tls Listen
	if ls := v.FieldByName("Listener"); ls.IsValid() {
		for hasElem(ls) {
			ls = ls.Elem()
		}
		v = ls
	}
	if v.Kind() != reflect.Struct {
		return fmt.Errorf("Not supported by current Go version")
	}
	v = v.FieldByName("fd")
	if !v.IsValid() {
		return fmt.Errorf("Not supported by current Go version")
	}
	v = v.Elem()
	fdField := v.FieldByName("sysfd")
	if !fdField.IsValid() {
		fdField = v.FieldByName("pfd").FieldByName("Sysfd")
	}

	if !fdField.IsValid() {
		return fmt.Errorf("Not supported by current Go version")
	}
	var fd uintptr
	if runtime.GOOS == "windows" {
		fd = uintptr(fdField.Uint())
	} else {
		fd = uintptr(fdField.Int())
	}

	a.services.Store(name, &Service{
		Name:       name,
		FdName:     ListerName(ls),
		Listener:   ls,
		Descriptor: fd,
	})
	return nil
}

func (a Again) Get(name string) *Service {
	s, _ := a.services.Load(name)
	if s != nil {
		return s.(*Service)
	}
	return nil
}

func (a Again) Delete(name string) {
	a.services.Delete(name)
}

func (a Again) GetListener(key string) net.Listener {
	if s := a.Get(key); s != nil {
		return s.Listener
	}
	return nil
}

// Re-exec this same image without dropping the net.Listener.
func Exec(a *Again) error {
	var pid int
	fmt.Sscan(os.Getenv("GOAGAIN_PID"), &pid)
	if syscall.Getppid() == pid {
		return fmt.Errorf("goagain.Exec called by a child process")
	}
	argv0, err := lookPath()
	if nil != err {
		return err
	}
	if err := setEnvs(a); nil != err {
		return err
	}
	if err := os.Setenv(
		"GOAGAIN_SIGNAL",
		fmt.Sprintf("%d", syscall.SIGQUIT),
	); nil != err {
		return err
	}
	log.Println("re-executing", argv0)
	return syscall.Exec(argv0, os.Args, os.Environ())
}

// Fork and exec this same image without dropping the net.Listener.
func ForkExec(a *Again) error {
	argv0, err := lookPath()
	if nil != err {
		return err
	}
	wd, err := os.Getwd()
	if nil != err {
		return err
	}
	err = setEnvs(a)
	if nil != err {
		return err
	}
	if err := os.Setenv("GOAGAIN_PID", ""); nil != err {
		return err
	}
	if err := os.Setenv(
		"GOAGAIN_PPID",
		fmt.Sprint(syscall.Getpid()),
	); nil != err {
		return err
	}

	sig := syscall.SIGQUIT
	if err := os.Setenv("GOAGAIN_SIGNAL", fmt.Sprintf("%d", sig)); nil != err {
		return err
	}

	files := []*os.File{
		os.Stdin, os.Stdout, os.Stderr,
	}
	a.Range(func(s *Service) {
		files = append(files, os.NewFile(
			s.Descriptor,
			ListerName(s.Listener),
		))
	})
	p, err := os.StartProcess(argv0, os.Args, &os.ProcAttr{
		Dir:   wd,
		Env:   os.Environ(),
		Files: files,
		Sys:   &syscall.SysProcAttr{},
	})
	if nil != err {
		return err
	}
	log.Println("spawned child", p.Pid)
	if err = os.Setenv("GOAGAIN_PID", fmt.Sprint(p.Pid)); nil != err {
		return err
	}
	return nil
}

// IsErrClosing tests whether an error is equivalent to net.errClosing as returned by
// Accept during a graceful exit.
func IsErrClosing(err error) bool {
	if opErr, ok := err.(*net.OpError); ok {
		err = opErr.Err
	}
	return "use of closed network connection" == err.Error()
}

// Child returns true if this process is managed by again and its a child
// process.
func Child() bool {
	d := os.Getenv("GOAGAIN_PID")
	if d == "" {
		d = os.Getenv("GOAGAIN_PPID")
	}
	var pid int
	_, err := fmt.Sscan(d, &pid)
	return err == nil
}

// Listen checks env and constructs a Again instance if this is a child process
// that was froked by again parent.
//
// forkHook if provided will be called before forking.
func Listen(forkHook func()) (*Again, error) {
	a := New()
	if err := ListenFrom(&a, forkHook); err != nil {
		return nil, err
	}
	return &a, nil
}

func ListenFrom(a *Again, forkHook func()) error {
	OnForkHook = forkHook
	fds := strings.Split(os.Getenv("GOAGAIN_FD"), ",")
	names := strings.Split(os.Getenv("GOAGAIN_SERVICE_NAME"), ",")
	fdNames := strings.Split(os.Getenv("GOAGAIN_NAME"), ",")
	if !((len(fds) == len(names)) && (len(fds) == len(fdNames))) {
		errors.New(("again: names/fds mismatch"))
	}
	for k, f := range fds {
		if f == "" {
			continue
		}
		var s Service
		_, err := fmt.Sscan(f, &s.Descriptor)
		if err != nil {
			return err
		}
		s.Name = names[k]
		s.FdName = fdNames[k]
		l, err := net.FileListener(os.NewFile(s.Descriptor, s.FdName))
		if err != nil {
			return err
		}
		s.Listener = l
		switch l.(type) {
		case *net.TCPListener, *net.UnixListener:
		default:
			return fmt.Errorf(
				"file descriptor is %T not *net.TCPListener or *net.UnixListener",
				l,
			)
		}
		if err = syscall.Close(syscall.Handle(s.Descriptor)); nil != err {
			return err
		}
		fmt.Println("=> ", s.Name, s.FdName)
		a.services.Store(s.Name, &s)
	}
	return nil
}

func lookPath() (argv0 string, err error) {
	argv0, err = exec.LookPath(os.Args[0])
	if nil != err {
		return
	}
	if _, err = os.Stat(argv0); nil != err {
		return
	}
	return
}

func setEnvs(a *Again) error {
	e, err := a.Env()
	if err != nil {
		return err
	}
	for k, v := range e {
		os.Setenv(k, v)
	}
	return nil
}
