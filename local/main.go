package main

import (
	"bufio"
	"context"
	"fmt"
	"github.com/projectcalico/calico/goldmane/pkg/client"
	"github.com/projectcalico/calico/goldmane/proto"
	"github.com/projectcalico/calico/whisker-backend/test/utils/container"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"
)

func setup() (context.Context, func()) {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt)

	// Use a channel to detect when the test is done
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		select {
		case <-sigs:
			fmt.Println("Interrupt received, ensuring cleanup...")
			// If interrupted, call t.Fail() to stop the test gracefully
		case <-ctx.Done():
			// If the test finishes naturally, return
		}
	}()
	return ctx, cancel
}

func main() {
	ctx, teardown := setup()
	defer teardown()

	goldmaneCtr := container.Container{
		ImageName: "calico/goldmane:master",
		EnvVars: []string{
			"LOG_LEVEL=debug",
			"ROLLOVER_INTERVAL=5s",
		},
	}

	if err := goldmaneCtr.Start(); err != nil {
		panic(err)
	}
	defer goldmaneCtr.Stop()

	whiskerCtr := container.Container{
		ImageName: "calico/whisker-backend:master",
		EnvVars: []string{
			"LOG_LEVEL=debug",
			"GOLDMANE_HOST=" + goldmaneCtr.Addr + ":443",
		},
	}

	if err := whiskerCtr.Start(); err != nil {
		panic(err)
	}
	defer whiskerCtr.Stop()

	cli := client.NewFlowClient(goldmaneCtr.Addr + ":443")

	// Wait for initial connection
	cli.Connect(ctx)

	resp, err := http.Get("http://" + whiskerCtr.Addr + ":8080/flows?watch=true")
	if err != nil {
		panic(err)
	}
	go func() {
		<-ctx.Done()
		resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		panic(err)
	}

	scanner := bufio.NewScanner(resp.Body)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()

		for scanner.Scan() {
			line := scanner.Text()

			// Handle SSE specific lines (e.g., `data:`).
			if strings.HasPrefix(line, "data:") {
				// Extract and print the event data.
				data := strings.TrimPrefix(line, "data:")
				fmt.Println("Event Data: ", strings.TrimSpace(data))
			} else if line == "" {
				// An empty line signifies the end of an event.
				fmt.Println("End of event")
			}
			return
		}
	}()

	cli.Push(&proto.Flow{
		Key: &proto.FlowKey{
			SourceName:      "test-source-2",
			SourceNamespace: "test-namespace-2",
		},
		StartTime: time.Now().Add(-1 * time.Second).Unix(),
		EndTime:   time.Now().Unix(),
	})

	wg.Wait()
}
