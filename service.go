package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"

	sas "github.com/Azure/azure-amqp-common-go/sas"
	eventhubs "github.com/Azure/azure-event-hubs-go"
)

type serviceConfiguration struct {
	ConfigurationBlobLocation string
	EventHubConnection        eventHubConfiguration
	TelemetryFrequency        int
	ContainerID               string
}

type eventHubConfiguration struct {
	Namespace          string
	Name               string
	SharedAccessPolicy string
	SharedAccessKey    string
}

type channelConfiguration struct {
	TemplateName string                 `json:"template_name"`
	ChannelName  string                 `json:"channel_name"`
	ChannelID    string                 `json:"channel_id"`
	AppData      map[string]interface{} `json:"app_data"`
	Inputs       []channelIO
	Outputs      []channelIO
}

type channelIO struct {
	ID         string
	Name       string
	URL        string
	Protocol   string
	Parameters []interface{}
}

func dropCR(data []byte) []byte {
	if len(data) > 0 && data[len(data)-1] == '\r' {
		return data[0 : len(data)-1]
	}
	return data
}

// Custom splitter that accepts line terminated with CR only.
// This is needed because ffmpeg likes to log stats
// on a single line (no new line present)
func scanLines(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		// We have a full newline-terminated line.
		return i + 1, dropCR(data[0:i]), nil
	}
	if i := bytes.IndexByte(data, '\r'); i >= 0 {
		// Will take CR-terminated too....
		return i + 1, data[0:i], nil
	}
	// If we're at EOF, we have a final, non-terminated line. Return it.
	if atEOF {
		return len(data), dropCR(data), nil
	}
	// Request more data.
	return 0, nil, nil
}

func pipeReader(r io.ReadCloser, wantNL bool, ch chan string) {
	scanner := bufio.NewScanner(r)
	if !wantNL {
		scanner.Split(scanLines)
	}
	for scanner.Scan() {
		ch <- scanner.Text()
	}
	close(ch)
}

func ffmpegRun(args string) {
	cmd := exec.Command("ffmpeg", "-progress", "-")
	cmd.Args = append(cmd.Args, strings.Split(args, " ")...)
	log.Printf("CMD: %v", &cmd.Args)

	// grab stdout and stderr pipes.
	// stdout is where the progress report will go
	// stderr is for general logging
	var err error
	var stdoutPipe, stderrPipe io.ReadCloser
	if stdoutPipe, err = cmd.StdoutPipe(); err != nil {
		log.Fatalf("Failed to connect stdout pipe: %v", err)
	}
	stdoutCh := make(chan string)
	go pipeReader(stdoutPipe, true, stdoutCh)

	if stderrPipe, err = cmd.StderrPipe(); err != nil {
		log.Fatalf("Failed to connect stderr pipe: %v", err)
	}
	stderrCh := make(chan string)
	go pipeReader(stderrPipe, false, stderrCh)

	// start ffmpeg in background
	if err := cmd.Start(); err != nil {
		log.Fatalf("Failed to start ffmpeg: %s", err)
	}

readloop:
	for {
		select {
		case stdout, ok := <-stdoutCh:
			if !ok {
				break readloop
			}
			fmt.Printf("LINE: %s\n", stdout)
		case stderr, ok := <-stderrCh:
			if !ok {
				break readloop
			}
			log.Println(stderr)
		}
	}
	log.Println("waiting for ffmpeg to exit...")
	err = cmd.Wait()

	log.Printf("ffmpeg existed with error: %v", err)
}

func main() {
	log.Println("Service is starting")

	var logJSON = flag.Bool("logJson", false, "Log JSON structures")
	flag.Parse()

	// Get service configuration
	serviceConfJSON, ok := os.LookupEnv("SERVICECONF")
	if !ok {
		log.Fatalln("Service configuration is not set")
	}
	if *logJSON {
		log.Printf("Found service configuration:\n%v\n", serviceConfJSON)
	}

	var serviceConf serviceConfiguration
	if err := json.Unmarshal([]byte(serviceConfJSON), &serviceConf); err != nil {
		log.Fatalf("Error decoding service configuration: %s", err)
	}
	fmt.Println(serviceConf)
	// Get channel configuration
	channelConfJSON, ok := os.LookupEnv("CHANNELCONF")
	if !ok {
		log.Fatalln("Channel configuration is not set")
	}
	if *logJSON {
		log.Printf("Found channel configuration:\n%v\n", channelConfJSON)
	}

	var channelConf channelConfiguration
	if err := json.Unmarshal([]byte(channelConfJSON), &channelConf); err != nil {
		log.Fatalf("Error decoding service configuration: %s", err)
	}

	// Create eventhub object
	tokenProvider, err := sas.NewTokenProvider(sas.TokenProviderWithKey(
		serviceConf.EventHubConnection.SharedAccessPolicy,
		serviceConf.EventHubConnection.SharedAccessKey))
	if err != nil {
		log.Fatalf("Failed to create token provider: %s\n", err)
	}

	hub, err := eventhubs.NewHub(
		serviceConf.EventHubConnection.Namespace,
		serviceConf.EventHubConnection.Name, tokenProvider)

	ctx := context.Background()
	defer hub.Close(ctx)
	if err != nil {
		log.Fatalf("Failed to create hub object: %v\n", err)
	}

	// Test eventhub object
	if false {
		info, err := hub.GetRuntimeInformation(ctx)
		if err != nil {
			log.Fatalf("Failed to get runtime info: %s\n", err)
		}
		log.Printf("Got partition IDs: %s\n", info.PartitionIDs)
		if err := hub.Send(ctx,
			eventhubs.NewEventFromString("hello Azure!")); err != nil {
			log.Fatalf("Failed to send test event: %s\n", err)
		}
	}

	// Start channel
	log.Printf("Starting channel '%s', ID '%s'",
		channelConf.ChannelName, channelConf.ChannelID)

	numInputs := len(channelConf.Inputs)
	if numInputs == 0 {
		log.Fatalln("No inputs specified, abort")
	}

	if numInputs != 1 {
		log.Printf(
			"WARNING: Only first input out of %d will be used\n", numInputs)
	}

	input := &channelConf.Inputs[0]
	log.Printf("INPUT '%s', ID '%s', URL '%s'",
		input.Name, input.ID, input.URL)

	numOutputs := len(channelConf.Outputs)
	if numOutputs == 0 {
		log.Fatalln("No outputs specified, abort")
	}

	for i := 0; i < numOutputs; i++ {
		output := &channelConf.Outputs[i]
		log.Printf("OUTPUT '%s', ID '%s', URL '%s'",
			output.Name, output.ID, output.URL)
	}

	ffmpegRun("-i udp://239.42.42.42:4242 -f mpegts udp://239.42.42.42:4243")
}
