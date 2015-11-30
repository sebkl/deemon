package deemon

import (
	"fmt"
	"os"
	"testing"
	"time"
)

func TestAll(t *testing.T) {
	ctx := NewContext(func() error {
		for {
			time.Sleep(time.Second * 1)
		}
	}, ReturnHandlerFunc(func(err error) error {
		return fmt.Errorf("stop return test.")
	}), ExitHandlerFunc(func(state *os.ProcessState) error {
		return fmt.Errorf("stop exit test.")
	}))

	err := ctx.Command("start")
	if err != nil {
		t.Errorf("deemon start command failed: %s", err)
	}

	time.Sleep(time.Second * 1)
	err = ctx.Command("status")
	if err != nil {
		t.Errorf("deemon start command failed: %s", err)
	}

	ctx.Command("stop")
	time.Sleep(time.Second * 1)
	err = ctx.Command("status")
	if err == nil {
		t.Errorf("deemon stop command failed: service is still, running.")
	}
}
