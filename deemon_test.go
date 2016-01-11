package deemon

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"testing"
	"time"
)

func TestAll(t *testing.T) {
	ctx := NewContext(func() error {
		for {
			log.Printf("Tick")

			for i := 0; i < 1024; i++ {
				log.Printf("%s", string((bytes.Repeat([]byte{'.'}, 1024))))
			}

			time.Sleep(time.Second * 1)
		}
	}, ReturnHandlerFunc(func(err error) error {
		return fmt.Errorf("stop return test.")
	}), ExitHandlerFunc(func(state *os.ProcessState) error {
		return fmt.Errorf("stop exit test.")
	}))
	ctx.RotateSize = 1

	err := ctx.Command("start")
	if err != nil {
		t.Errorf("deemon start command failed: %s", err)
	}

	time.Sleep(time.Second * 2)
	err = ctx.Command("status")
	if err != nil {
		t.Errorf("deemon start command failed: %s", err)
	}

	_, err = os.Stat(ctx.Logfile + ".1.gz")
	if err != nil {
		t.Errorf("Logfile was not rotated.")
	}

	ctx.Command("stop")
	time.Sleep(time.Second * 1)
	err = ctx.Command("status")
	if err == nil {
		t.Errorf("deemon stop command failed: service is still, running.")
	}
}
