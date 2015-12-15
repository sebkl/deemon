package deemon

import (
	"fmt"
	//d "github.com/sevlyar/go-daemon"
	"bytes"
	"flag"
	"io/ioutil"
	"log"
	"os"
	"os/signal"
	"path"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	MARK_KEY            = "_DEEMON_PROC_TYPE_"
	MARK_CHILD          = "CHILD"
	MARK_WATCHDOG       = "WATCHDOG"
	MARK_STARTER        = "STARTER"
	DefaultMaxKillRetry = 10
)

type Config struct {
	Pidfile string
	Logfile string
	Workdir string
}

var defaultConfig Config

func init() {
	flag.StringVar(&defaultConfig.Pidfile, "pidfile", "", "Location of the pidfile.")
	flag.StringVar(&defaultConfig.Logfile, "logfile", "", "Location of the logfile.")
	flag.StringVar(&defaultConfig.Workdir, "workdir", "", "Location of the working directory to change to.")
}

// The loop is only restarted if none of the Handler implementations return an error != nil.
type StartFunc func() error
type ReturnHandlerFunc func(error) error
type SignalHandlerFunc func(os.Signal) error
type PanicHandlerFunc func(interface{}) error
type ExitHandlerFunc func(*os.ProcessState) error
type AnyHandlerFunc func(error) error

type Context struct {
	Config

	UseWatchdog  bool
	MaxKillRetry int

	OnExit   ExitHandlerFunc
	OnPanic  PanicHandlerFunc
	OnSignal SignalHandlerFunc
	OnReturn ReturnHandlerFunc
	OnAny    AnyHandlerFunc

	DefaultOnExit   ExitHandlerFunc
	DefaultOnPanic  PanicHandlerFunc
	DefaultOnSignal SignalHandlerFunc
	DefaultOnReturn ReturnHandlerFunc
	DefaultOnAny    AnyHandlerFunc

	Start StartFunc

	Name     string
	MustRun  time.Duration
	ptype    string
	rchild   *os.Process
	watchdog *os.Process
	lf       *os.File
}

func Launch(s StartFunc, args ...interface{}) (ctx *Context, err error) {
	c := NewContext(s, args...)
	err = c.Launch()
	if err != nil {
		c.Logf("error: %s", err)
	}
	return c, err
}

func NewContext(s StartFunc, args ...interface{}) *Context {

	//determin which kind of proc this is.
	ptype := os.Getenv(MARK_KEY)
	if len(ptype) == 0 {
		ptype = MARK_STARTER
	}

	name := path.Base(os.Args[0])

	var ret *Context
	ret = &Context{
		Config:       defaultConfig,
		rchild:       nil,
		ptype:        ptype,
		Name:         name,
		MaxKillRetry: DefaultMaxKillRetry,
		DefaultOnExit: func(state *os.ProcessState) error {
			ret.Logf("onExit %s", state.String())
			return nil
		},
		DefaultOnPanic: func(i interface{}) error {
			ret.Logf("onPanic %s", i)
			debug.PrintStack()
			return nil
		},
		DefaultOnReturn: func(err error) error {
			ret.Logf("onReturn %s", err)
			return nil
		},
		DefaultOnSignal: func(sig os.Signal) error {
			ret.Logf("onSignal %s", sig)
			return ret.Errorf("exiting due to sig %s", sig)
		},
		DefaultOnAny: func(err error) error {
			return err
		},
		Start:   s,
		MustRun: time.Second * 1,
	}

	if len(ret.Config.Pidfile) <= 0 {
		ret.Config.Pidfile = name + ".pid"
	}

	if len(ret.Config.Logfile) <= 0 {
		ret.Config.Logfile = name + ".log"
	}

	ret.OnExit = ret.DefaultOnExit
	ret.OnPanic = ret.DefaultOnPanic
	ret.OnReturn = ret.DefaultOnReturn
	ret.OnSignal = ret.DefaultOnSignal
	ret.OnAny = ret.DefaultOnAny

	for _, m := range args {
		if v, ok := m.(ReturnHandlerFunc); ok {
			ret.Logf("registering ReturnHandler")
			ret.OnReturn = v
		}

		if v, ok := m.(PanicHandlerFunc); ok {
			ret.Logf("registering PanicHandler")
			ret.OnPanic = v
		}

		if v, ok := m.(SignalHandlerFunc); ok {
			ret.Logf("registering SignalHandler")
			ret.OnSignal = v
		}

		if v, ok := m.(ExitHandlerFunc); ok {
			ret.Logf("registering ExitHandler")
			ret.OnExit = v
		}

		if v, ok := m.(AnyHandlerFunc); ok {
			ret.Logf("registering AnyHandler")
			ret.OnAny = v
		}
	}

	ret.readPidfile()
	return ret
}

func (c *Context) doChild() error {
	c.Logf("doChild")
	ret := make(chan error, 1)

	run := true
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGKILL, syscall.SIGTERM, syscall.SIGABRT)

	for run {
		laststart := time.Now()
		go func() {
			var err error
			defer func() {
				if r := recover(); r != nil {
					err = c.OnPanic(r)
				}
				ret <- err
			}()
			c.Logf("starting Start() routine")
			err = c.Start()
		}()

		var err error
		select {
		case err := <-ret: // START returned an error
			err = c.OnReturn(err)

		case sig := <-sc: // A Termination signal was catched
			err = c.OnSignal(sig)
		}
		err = c.OnAny(err)
		if err != nil {
			return c.Errorf("exiting due to error in handler: '%s'", err)
		}

		delta := time.Now().Sub(laststart)
		if delta < c.MustRun {
			log.Fatalf("returned within %.3f seconds. Assuming startup error. Aborted.", delta.Seconds())
		}

		c.Logf("restarting: %s %s", c.Name, c.ptype)
	}
	return nil
}

func (c *Context) doWatchdog() (err error) {
	proc, err := os.FindProcess(os.Getpid())
	if err != nil {
		return
	}
	c.watchdog = proc

	defer c.close()
	run := true

	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGKILL, syscall.SIGTERM, syscall.SIGABRT)
	go func() {
		sig := <-sc
		c.Logf("received signal %d", sig)
		run = false
		if c.rchild != nil {
			c.Logf("pass signal %d to PID=%d", sig, c.rchild.Pid)
			c.rchild.Signal(sig) //  Pass the same signal to the child
		}
	}()

	for run {
		laststart := time.Now()

		err = c.restartAs(MARK_CHILD)
		if err != nil {
			return
		}

		err = ioutil.WriteFile(c.Pidfile, []byte(fmt.Sprintf("%d\n%d", c.rchild.Pid, os.Getpid())), 0644)
		if err != nil {
			return
		}

		c.Logf("observing")
		state, err := c.rchild.Wait()
		if err != nil {
			return err
		}

		delta := time.Now().Sub(laststart)
		if delta < c.MustRun {
			return c.Errorf("child returned within %.3f seconds", delta.Seconds)
		}
		err = c.OnExit(state)
		if err != nil {
			err = c.Errorf("exiting due to error in 'onExit' handler: '%s'", err)
			log.Print(err)
			return err
		}

		if run {
			c.Logf("restarting child")
		}
	}
	return nil
}

func (c *Context) restartAs(mark string) (err error) {
	oe := os.Environ()
	ne := make([]string, 0)

	for _, e := range oe {
		if !strings.HasPrefix(e, MARK_KEY) {
			ne = append(ne, e)
		}
	}
	ne = append(ne, fmt.Sprintf("%s=%s", MARK_KEY, mark))

	attr := &os.ProcAttr{
		Dir: c.Config.Workdir,
		Env: ne,
		Files: []*os.File{
			os.Stdin,
			c.lf,
			c.lf,
		},
		Sys: &syscall.SysProcAttr{
			Setsid: true,
		},
	}

	c.Logf("restarting to %s", mark)
	child, err := os.StartProcess(os.Args[0], os.Args, attr)
	if err != nil {
		return err
	}

	if child == nil {
		return c.Errorf("child should never become a same proc type: %s,%s", c.Name, c.ptype)
	}
	c.rchild = child

	return err
}

func (c *Context) doStarter() (err error) {
	//TODO: check for UseWatchdog
	err = c.restartAs(MARK_WATCHDOG)
	if err != nil {
		return
	}

	c.Logf("Starter done. Please find logs at '%s'", c.Logfile)
	return nil
}

func (c *Context) close() error {
	c.lf.Close()
	return os.Remove(c.Pidfile)
}

func (c *Context) Launch() (err error) {
	c.lf, err = os.OpenFile(c.Config.Logfile, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0644)
	if err != nil {
		return err
	}

	/* route proc type */
	switch c.ptype {
	case MARK_CHILD:
		log.SetOutput(c.lf)
		err = c.doChild()
		c.Logf("Exiting: %s", err)
		os.Exit(0)
	case MARK_WATCHDOG:
		c.Logf("Starting. Writing logs to: '%s'", c.Logfile)
		log.SetOutput(c.lf)
		err = c.doWatchdog()
		c.Logf("Exiting: %s", err)
		os.Exit(0)
	case MARK_STARTER:
		return c.doStarter()
	default:
		return c.Errorf("unknown proc type: %s", c.ptype)
	}
	return nil
}

func (c *Context) Logf(pat string, args ...interface{}) {
	log.Printf("[%s|%s|%d] "+pat, append([]interface{}{c.Name, c.ptype, os.Getpid()}, args...)...)
}

func (c *Context) Errorf(pat string, args ...interface{}) error {
	return fmt.Errorf("[%s|%s|%d] "+pat, append([]interface{}{c.Name, c.ptype, os.Getpid()}, args...)...)
}

func (c *Context) readPidfile() error {
	c.rchild = nil
	c.watchdog = nil

	data, err := ioutil.ReadFile(c.Config.Pidfile)
	if err != nil {
		return nil
	}

	sb := bytes.Split(data, []byte{'\n'})
	if len(sb) >= 1 {
		cpid, err := strconv.Atoi(string(sb[0]))
		if err != nil {
			return err
		}

		proc, _ := os.FindProcess(cpid)
		c.rchild = proc
	}

	if len(sb) >= 2 {
		wdpid, err := strconv.Atoi(string(sb[1]))
		if err != nil {
			return err
		}
		proc, _ := os.FindProcess(wdpid)
		c.watchdog = proc
	}

	return nil
}

func (c *Context) Kill() error {
	return c.kill(syscall.SIGKILL)
}

func (c *Context) Stop() error {
	return c.kill(syscall.SIGTERM)
}

func (c *Context) kill(sig os.Signal) error {
	for i := 0; i < c.MaxKillRetry; i++ {
		c.readPidfile()

		if c.IsDown() {
			return nil
		}

		if c.watchdog != nil {
			c.Logf("Sending %s to watchdog PID=%d", sig, c.watchdog.Pid)
			c.watchdog.Signal(syscall.SIGTERM) // Never send the watchdog a kill
		}

		if c.rchild != nil {
			c.Logf("Sending %s to child PID=%d", sig, c.rchild.Pid)
			c.rchild.Signal(sig)
		}
		time.Sleep(time.Second * 1)
	}

	return c.Errorf("Could not stop process '%s' after %d retries.", c.Name, c.MaxKillRetry)
}

func (c *Context) IsRunning() bool {
	return (c.rchild != nil && c.watchdog != nil)
}

func (c *Context) IsDown() bool {
	return (c.rchild == nil && c.watchdog == nil)
}

func (c *Context) amITheChild() bool {
	if c.rchild != nil {
		return c.rchild.Pid == os.Getpid()
	}
	return false
}

func Command(cmd string, s StartFunc, fs ...interface{}) (ctx *Context, err error) {
	ctx = NewContext(s, fs...)
	if ctx == nil {
		return nil, fmt.Errorf("Could not create context.")
	}
	err = ctx.Command(cmd)
	return ctx, err
}

func PrintUsage() {
	fmt.Fprintf(os.Stderr, `
usage: %s [flags...] <target>

Targets:
--------
	start		Start the service.
	stop		Stop the service.
	kill		Kill the service.
	status		Print if service is running.

Flags:
------
`, os.Args[0])
	flag.PrintDefaults()
}

func (c *Context) Command(cmd string) (err error) {
	c.readPidfile()
	cm := strings.ToLower(cmd)
	running := c.IsRunning()
	switch cm {
	case "stop":
		if !running {
			return
		}
		return c.Stop()
	case "kill":
		if !running {
			return
		}
		return c.Kill()
	case "start":
		if !running || c.amITheChild() {
			return c.Launch()
		} else {
			c.Logf("Already Running")
			return c.Errorf("Service already running.")
		}
	case "status":
		if running {
			c.Logf("Service %s is running at PID=%d,%d\n", c.Name, c.watchdog.Pid, c.rchild.Pid)

			/* TODO: Send SIG INFO/STATUS to child process */
			return nil
		} else {
			return c.Errorf("Service %s is not running", c.Name)
		}
		flag.PrintDefaults()
	default:
		return c.Errorf("unknown command: %s", cmd)
	}
	return
}
