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
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	sas "github.com/Azure/azure-amqp-common-go/sas"
	eventhubs "github.com/Azure/azure-event-hubs-go"
	"github.com/shirou/gopsutil/process"
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
	Parameters []map[string]string
}

type containerStatus struct {
	Health           string          `json:"health"`
	HealthMessage    string          `json:"health_message"`
	Channels         []channelStatus `json:"channels"`
	CPUsage          float64         `json:"cpu_usage"`
	MemoryUsageRel   float64         `json:"memory_usage_rel"`
	BandwithUsageIn  uint64          `json:"bandwith_usage_in"`
	BandwithUsageOut uint64          `json:"bandwith_usage_out"`
	/*
		CPUUsageDetailed    []int         `json:"cpu_usage_detailed"`
		MemoryUsageAbsolute int           `json:"memory_usage_absolut"`
		MemoryPhy           int           `json:"memory_phy"`
	*/
}

type channelStatus struct {
	Name          string                 `json:"Name"`
	ID            string                 `json:"id"`
	Health        string                 `json:"health"`
	HealthMessage string                 `json:"health_message"`
	AppData       map[string]interface{} `json:"app_data"`
	Inputs        []channelIOStatus      `json:"inputs"`
	Outputs       []channelIOStatus      `json:"outputs"`
}

type channelIOStatus struct {
	ID             string         `json:"id"`
	Name           string         `json:"Name"`
	Health         string         `json:"health"`
	HealthMessage  string         `json:"health_message"`
	URL            string         `json:"url"`
	Protocol       string         `json:"protocol"`
	ProtocolStatus protocolStatus `json:"protocol_status"`
}

type protocolStatus struct {
	ConnectionState string  `json:"connection_state"`
	ReconnectCount  int     `json:"reconnect_count"`
	PayloadBitrate  float64 `json:"payload_bitrate"`
	/*
		UsedBandwidth   float64 `json:"used_bandwidth"`
	*/
}

type ffmpegProgress struct {
	frame          int
	fps            float64
	bitrate        float64
	totalSize      int
	outTimeMS      int
	dupFrames      int
	dropFrames     int
	cpuUsage       float64
	memoryUsageRel float64
	bytesSent      uint64
	bytesRecv      uint64
}

type serviceContext struct {
	ctx         context.Context
	serviceConf serviceConfiguration
	channelConf channelConfiguration
	progress    ffmpegProgress
}

func dropCR(data []byte) []byte {
	if len(data) > 0 && data[len(data)-1] == '\r' {
		return data[0 : len(data)-1]
	}
	return data
}

// Custom splitter that accepts line terminated with CR only.
// This is needed because ffmpeg likes to log stats without adding a new line.
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

func parseFFmpegProgress(progress *ffmpegProgress, kv []string) {
	switch kv[0] {
	case "frame":
		progress.frame, _ = strconv.Atoi(kv[1])
	case "fps":
		progress.fps, _ = strconv.ParseFloat(kv[1], 32)
		progress.fps = float64(int(progress.fps*100+0.5)) / 100
	case "bitrate":
		progress.bitrate, _ = strconv.ParseFloat(
			strings.TrimRight(kv[1], "kbits/s"), 32)
		progress.bitrate = float64(int(progress.bitrate*10+0.5)) / 10
	case "total_size":
		progress.totalSize, _ = strconv.Atoi(kv[1])
	case "out_time_ms":
		progress.outTimeMS, _ = strconv.Atoi(kv[1])
	case "dup_frames":
		progress.dupFrames, _ = strconv.Atoi(kv[1])
	case "drop_frames":
		progress.dupFrames, _ = strconv.Atoi(kv[1])
	}
}

func ffmpegRun(ctx context.Context, args []string) <-chan ffmpegProgress {
	cmd := exec.CommandContext(ctx,
		/* "/work/ffmpeg/_install/bin/ffmpeg" */ "ffmpeg", "-progress", "-",
		"-v", "verbose")

	cmd.Args = append(cmd.Args, args...)
	cmd.Env = append(cmd.Env, "LD_LIBRARY_PATH=/work/ffmpeg/_install/lib/")
	log.Printf("CMD: %v", &cmd.Args)

	// grab stdout and stderr pipes.
	// stdout is where the progress report will go
	// stderr is for general logging
	var err error
	var stdoutPipe, stderrPipe io.ReadCloser
	if stdoutPipe, err = cmd.StdoutPipe(); err != nil {
		log.Fatalln("Failed to connect stdout pipe: ", err)
	}
	stdoutCh := make(chan string)
	go pipeReader(stdoutPipe, true, stdoutCh)

	if stderrPipe, err = cmd.StderrPipe(); err != nil {
		log.Fatalln("Failed to connect stderr pipe: ", err)
	}
	stderrCh := make(chan string)
	go pipeReader(stderrPipe, false, stderrCh)

	// start ffmpeg in background
	if err := cmd.Start(); err != nil {
		log.Fatalln("Failed to start ffmpeg: ", err)
	}

	ch := make(chan ffmpegProgress)
	go func() {
		var progress ffmpegProgress
		log.Println("FFmpeg PID", cmd.Process.Pid)

		ticker := time.NewTicker(
			time.Duration(3) * time.Second)
		process, err := process.NewProcess(int32(cmd.Process.Pid))
		if err != nil {
			log.Println("Failed to find process", cmd.Process.Pid)
		}
		var cpuUsagePct, memUsagePct float64
		var bytesSent, bytesRecv uint64
		gotProgress := false
	readloop:
		for {
			select {
			case stdout, ok := <-stdoutCh:
				if !ok {
					break readloop
				}
				if strings.Contains(stdout, "progress=") {
					gotProgress = true
					if progress.frame != 0 {
						progress.cpuUsage = cpuUsagePct
						progress.memoryUsageRel = memUsagePct
						progress.bytesSent = bytesSent
						progress.bytesRecv = bytesRecv
						ch <- progress
					}
					progress = ffmpegProgress{}
				} else {
					if kv := strings.SplitN(stdout, "=", 2); len(kv) == 2 {
						parseFFmpegProgress(&progress, kv)
					}
				}
			case stderr, ok := <-stderrCh:
				if !ok {
					break readloop
				}
				log.Println(stderr)
			case <-ticker.C:
				if process != nil {
					cpuUsagePct, err = process.PercentWithContext(ctx, 0)
					if err == nil {
						cpuUsagePct = float64(int(cpuUsagePct*10+0.5)) / 10
					}
					v, err := process.MemoryPercentWithContext(ctx)
					if err == nil {
						memUsagePct = float64(int(v*10+0.5)) / 10
					}
					ioCounts, err := process.NetIOCountersWithContext(ctx, false)
					if err == nil {
						bytesSent = ioCounts[0].BytesSent
						bytesRecv = ioCounts[0].BytesRecv
					}
					// if we don't have ffmpeg progress report yet,
					// send out hosts stats anyway
					if !gotProgress {
						progress.cpuUsage = cpuUsagePct
						progress.memoryUsageRel = memUsagePct
						progress.bytesSent = bytesSent
						progress.bytesRecv = bytesRecv
						ch <- progress
					}
					if false {
						log.Println(
							"CPU", cpuUsagePct,
							"MEM", memUsagePct,
							"OUT", bytesSent,
							"IN", bytesRecv)
					}
				}
			case <-ctx.Done():
				break readloop
			}
		}
		log.Println("waiting for ffmpeg to exit...")
		err = cmd.Wait()
		log.Printf("ffmpeg exited with error: %v", err)

		close(ch)
	}()

	return ch
}

func createStatusReport(serviceConf *serviceConfiguration,
	channelConf *channelConfiguration,
	progress *ffmpegProgress, restartCount int) ([]byte, error) {

	var status containerStatus
	status.Health = "ok"
	status.CPUsage = progress.cpuUsage
	status.MemoryUsageRel = progress.memoryUsageRel
	status.BandwithUsageIn = progress.bytesRecv
	status.BandwithUsageOut = progress.bytesSent

	status.Channels = make([]channelStatus, 1)
	ch := &status.Channels[0]
	ch.Name = channelConf.ChannelName
	ch.ID = channelConf.ChannelID
	ch.Health = "ok"
	ch.AppData = channelConf.AppData

	ch.Inputs = make([]channelIOStatus, 1)
	in := &ch.Inputs[0]
	in.ID = channelConf.Inputs[0].ID
	in.Name = channelConf.Inputs[0].Name
	in.Health = "ok"
	in.URL = channelConf.Inputs[0].URL
	in.Protocol = channelConf.Inputs[0].Protocol
	switch {
	case progress.frame > 0:
		in.ProtocolStatus.ConnectionState = "connected"
	default:
		in.ProtocolStatus.ConnectionState = "disconnected"
	}
	in.ProtocolStatus.ReconnectCount = restartCount

	ch.Outputs = make([]channelIOStatus, 1)
	out := &ch.Outputs[0]
	out.ID = channelConf.Outputs[0].ID
	out.Name = channelConf.Outputs[0].Name
	out.Health = "ok"
	out.URL = channelConf.Outputs[0].URL
	out.Protocol = channelConf.Outputs[0].Protocol
	switch {
	case progress.frame > 0:
		out.ProtocolStatus.ConnectionState = "connected"
	default:
		out.ProtocolStatus.ConnectionState = "disconnected"
	}

	out.ProtocolStatus.PayloadBitrate = progress.bitrate
	out.ProtocolStatus.ReconnectCount = restartCount

	return json.Marshal(status)
}

func parseSRTParameters(b *[]string, ps *[]map[string]string) {
	for _, p := range *ps {
		k := p["name"]
		v := p["value"]

		if len(v) == 0 {
			continue
		}

		switch k {
		case "srt_mode":
			*b = append(*b, "-mode")
			*b = append(*b, v)
		case "srt_encryption":
			var l string
			switch v {
			case "AES128":
				l = "16"
			case "AES192":
				l = "24"
			case "AES256":
				l = "32"
			}
			if len(l) != 0 {
				*b = append(*b, "-pbkeylen")
				*b = append(*b, l)
			}
		case "srt_passphrase":
			*b = append(*b, "-passphrase")
			*b = append(*b, v)
		case "srt_latency":
			*b = append(*b, "-latency")
			*b = append(*b, v)
		case "srt_mss":
			*b = append(*b, "-mss")
			*b = append(*b, v)
		case "srt_overheadbw":
			*b = append(*b, "-oheadbw")
			*b = append(*b, v)
		case "srt_maxbw":
			*b = append(*b, "-maxbw")
			*b = append(*b, v)
		}
	}
}

func createFFmpegInvocation(channelConf *channelConfiguration) []string {
	b := make([]string, 0, 100)

	input := &channelConf.Inputs[0]
	output := &channelConf.Outputs[0]

	if true {
		log.Printf("INPUT '%s', ID '%s', URL '%s'",
			input.Name, input.ID, input.URL)
		log.Printf("OUTPUT '%s', ID '%s', URL '%s'",
			output.Name, output.ID, output.URL)
	}

	convertAudio := false
	var format string
	switch output.Protocol {
	case "srt":
		format = "mpegts"
	case "rtmp":
		convertAudio = true
		format = "flv"
	case "rtmps":
		convertAudio = true
		format = "flv"
	case "udp":
		format = "mpegts"
	case "rtp":
		format = "rtp"
	case "rtsp":
		format = "rtsp"
		/*
			case "hls":
				format = "mpegts"
			case "dash":
				format = "mpegts"
		*/
	default:
		log.Fatal("Unsupported output protocol", output.Protocol)
	}

	switch input.Protocol {
	case "srt":
		parseSRTParameters(&b, &input.Parameters)
	case "rtmp":
		convertAudio = false
	case "rtmps":
		convertAudio = false
	case "udp":
	case "rtp":
	case "rtsp":
	case "hls":
	case "dash":
	default:
		log.Fatal("Unsupported input protocol", input.Protocol)
	}

	b = append(b, "-i")
	b = append(b, input.URL)

	if convertAudio {
		b = append(b, strings.Fields(
			"-c:a aac -b:a 128k -ar 44100 -c:v copy")...)
	} else {
		b = append(b, strings.Fields(
			"-codec copy")...)
	}
	switch output.Protocol {
	case "srt":
		parseSRTParameters(&b, &output.Parameters)
	case "udp":
		// workaround ffmpeg sending out huge UDP packets
		if true {
			b = append(b, "-pkt_size")
			b = append(b, "1316")
		}
	}

	b = append(b, "-f")
	b = append(b, format)
	b = append(b, output.URL)

	return b
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
		log.Fatalln("Error decoding service configuration: ", err)
	}

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
		log.Fatalln("Error decoding service configuration: ", err)
	}

	// Create eventhub object
	tokenProvider, err := sas.NewTokenProvider(sas.TokenProviderWithKey(
		serviceConf.EventHubConnection.SharedAccessPolicy,
		serviceConf.EventHubConnection.SharedAccessKey))
	if err != nil {
		log.Fatalln("Failed to create token provider: ", err)
	}

	hub, err := eventhubs.NewHub(
		serviceConf.EventHubConnection.Namespace,
		serviceConf.EventHubConnection.Name, tokenProvider)

	ctx := context.Background()
	defer hub.Close(ctx)
	if err != nil {
		log.Fatalln("Failed to create hub object: ", err)
	}

	// Test eventhub object
	if true {
		info, err := hub.GetRuntimeInformation(ctx)
		if err != nil {
			log.Fatalln("Failed to get runtime info: ", err)
		}
		log.Println("Got partition IDs: ", info.PartitionIDs)
		if err := hub.Send(ctx,
			eventhubs.NewEventFromString("hello Azure!")); err != nil {
			log.Fatalln("Failed to send test event: ", err)
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

	numOutputs := len(channelConf.Outputs)
	if numOutputs == 0 {
		log.Fatalln("No outputs specified, abort")
	}

	if numOutputs != 1 {
		log.Printf(
			"WARNING: Only first output out of %d will be used\n", numOutputs)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var chProgress <-chan ffmpegProgress
	var progress ffmpegProgress
	needRestart := true
	restartCount := 0

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGUSR1)

	ticker := &time.Ticker{}
	if serviceConf.TelemetryFrequency > 0 {
		ticker = time.NewTicker(
			time.Duration(serviceConf.TelemetryFrequency) * time.Second)
	}
	for ctx.Err() == nil {
		if needRestart {
			s := createFFmpegInvocation(&channelConf)
			chProgress = ffmpegRun(ctx, s)
			needRestart = false
		}

		select {
		case progress, ok = <-chProgress:
			if !ok {
				// throttle restart
				time.Sleep(1 * time.Second)
				needRestart = true
				restartCount++
				break
			}
		case <-ticker.C:
			r, err := createStatusReport(&serviceConf, &channelConf,
				&progress, restartCount)
			if err != nil {
				log.Println("Error creating status report:", err)
			} else {
				e := eventhubs.NewEvent(r)
				e.Set("componentType", "container")
				e.Set("componentId", serviceConf.ContainerID)
				err = hub.Send(ctx, e)
				if err != nil {
					log.Println("WARNING: Failed to send event", err)
				}
			}
		case s := <-signals:
			log.Println("Caught", s)
			if s == syscall.SIGUSR1 {
				r, err := createStatusReport(&serviceConf, &channelConf,
					&progress, restartCount)
				if err != nil {
					log.Println("Error creating status report:", err)
				} else {
					fmt.Println(string(r))
				}
			} else {
				cancel()
			}
		}
	}
	log.Println("Waiting for processes to terminate...")
	select {
	case _, ok = <-chProgress:
		if !ok {
			log.Println("...Done!")
		}
	case <-time.After(5 * time.Second):
		log.Println("...Timed out")
	}
}
