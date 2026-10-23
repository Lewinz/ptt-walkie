// Command ptt-sim runs headless walkie-talkie devices against a ptt-server,
// speaking the same protocol as the firmware (docs/protocol.md).
//
//	ptt-sim -mode echo -code 3825                 # radio-check bot: repeats what it hears
//	ptt-sim -mode play -code 3825 -file a.ogg     # transmits an Ogg Opus file every -every
//	ptt-sim -mode load -code 3825 -n 50           # fan-out latency / loss benchmark
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"time"

	"github.com/plant-squad/ptt-walkie/internal/sim"
)

func main() {
	server := flag.String("server", "http://localhost:8080", "ptt-server base URL")
	mode := flag.String("mode", "echo", "echo | play | load")
	code := flag.String("code", "", "room code to join (required)")
	device := flag.String("device", "sim-echo", "device id (load mode uses it as a prefix)")
	name := flag.String("name", "回声测试", "display name")
	file := flag.String("file", "", "Ogg Opus file to transmit (play mode)")
	every := flag.Duration("every", 20*time.Second, "interval between transmissions (play mode)")
	n := flag.Int("n", 20, "number of listening devices (load mode)")
	dur := flag.Duration("dur", 10*time.Second, "talk duration (load mode)")
	flag.Parse()
	if *code == "" {
		flag.Usage()
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	var err error
	switch *mode {
	case "echo":
		err = sim.RunEcho(ctx, *server, *device, *name, *code)
	case "play":
		err = sim.RunPlay(ctx, *server, *device, *name, *code, *file, *every)
	case "load":
		var rep sim.LoadReport
		if rep, err = sim.RunLoad(ctx, *server, *device, *code, *n, *dur); err == nil {
			fmt.Println(rep)
		}
	default:
		err = fmt.Errorf("unknown mode %q", *mode)
	}
	if err != nil && ctx.Err() == nil {
		log.Fatal(err)
	}
}
