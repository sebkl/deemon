# deemon
Packge deemon is a small **experimental** package that aims to simplify creating daemon processes in GO. It is inspired by [sevlyar/go-daemon](https://github.com/sevylar/go-daemon) but focuses more on recovering a crashed process and supporting a *rc.d* compatible **start,stop,restart,status** interface.

## Usage

```go

ctx, _ := Launch(
	StartFunc(func() error { 
		/* The main loop of the service/daemon */
		for {
			time.Sleep(time.Millisecond * 100)
			panic("test")
		}
	}), ReturnHandlerFunc(func(err error) error {
		/* what do do if the main loop returns */
		return nil
	}), ExitHandlerFunc(func(state *os.ProcessState) error {
		/* What to do if the child process exits */
		return nil
	}), PanicHandlerFunc(func(i interface{}) error {
		/* What do do if the main loop panics */
		return nil
	}))

```

## TODO
- add examples
- add more testcase. SEGFAULT (CGO) handling in particular.
- do SIGKILL after SIGTERM tries.
- add SIGINFO 
- redirect stdout to logfile/ not just the log package


##Documentation
As usual: [sebkl/deemon](https://godoc.org/github.com/sebkl/deemon)
